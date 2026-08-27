#!/usr/bin/env python3
"""End-to-end integration test of claude-suspend / claude-resume against a
REAL Claude Code binary — with NO credentials and NO network (issue #23).

Claude is pointed at ci/fake_anthropic.py via ANTHROPIC_BASE_URL with a
dummy ANTHROPIC_API_KEY, inside a fresh $HOME pre-seeded to skip
onboarding. Probing showed Claude Code makes zero API requests across this
whole lifecycle (start → idle → /exit → --resume), so the fake server is a
safety net, and the suspend/resume functionality under test is asserted to
work regardless of what the API returns; any request that does arrive is
logged and reported.

Flow: start Claude in a tmux window → wait for its prompt (answering the
folder-trust dialog) → `go-tmux-saver claude-suspend` it → assert the
/exit-confirm-placeholder round-trip, the saved-output capture, and that a
`save` records the same session id → press Enter on the placeholder →
assert Claude relaunches → suspend again, then tear the server down.

Requirements: tmux, a `claude` binary on PATH, GTS_BIN pointing at the
go-tmux-saver build under test. Stdlib only.
"""
import json
import os
import subprocess
import sys
import time
from pathlib import Path

import fake_anthropic

SOCK = os.environ.get("GTS_TEST_SOCKET", f"gts-ci-{os.getpid()}")
GTS = os.environ["GTS_BIN"]
DEADLINE = float(os.environ.get("GTS_TEST_TIMEOUT", "120"))
DUMMY_KEY = "sk-ant-api03-" + "m" * 80 + "-mockmockAA"

WORK = Path(os.environ.get("GTS_TEST_DIR", "/tmp")) / f"gts-ci-{os.getpid()}"
HOME = WORK / "home"          # fresh: never the runner's real ~/.claude
PROJ = HOME / "demo-project"  # NOT nested in any repo: claude walks parent
                              # dirs for project config/trust state
DATA = WORK / "store"
CFG = WORK / "config.json"
API_LOG = WORK / "fake-api-requests.log"


def log(msg):
    print(f"--- {msg}", flush=True)


def tmux(*args, check=True):
    r = subprocess.run(["tmux", "-L", SOCK, "-f", "/dev/null", *args],
                       capture_output=True, text=True)
    if check and r.returncode != 0:
        sys.exit(f"tmux {args} failed: {r.stderr}")
    return r


def gts(cmd, *rest):
    # Flags before positionals (stdlib flag parsing); HOME is the fresh one
    # so the suspend's registry lookup sees the same world as the pane.
    return subprocess.run([GTS, cmd, "--socket", SOCK,
                           "--config", str(CFG), "--data-dir", str(DATA), *rest],
                          capture_output=True, text=True,
                          env={**os.environ, "HOME": str(HOME)})


def pane_text(target="=default:1"):
    return tmux("capture-pane", "-pJ", "-t", target).stdout


def wait_for(desc, pred, timeout=DEADLINE, interval=0.5):
    t0 = time.time()
    while time.time() - t0 < timeout:
        v = pred()
        if v:
            log(f"{desc}: ok ({time.time() - t0:.1f}s)")
            return v
        time.sleep(interval)
    sys.exit(f"TIMEOUT waiting for {desc}\n--- pane ---\n{pane_text()}")


def claude_pids():
    out = subprocess.run(["pgrep", "-x", "claude"], capture_output=True, text=True)
    return [int(p) for p in out.stdout.split()]


def answer_dialogs():
    """Answer Claude's first-run dialogs with Enter until the input prompt
    appears. Dialogs are checked FIRST, and a bare ❯ is never treated as
    the ready signal: selection dialogs draw ❯ as their cursor arrow, so
    matching it as "prompt ready" leaves the trust dialog unanswered
    forever. Ready = the status-line hints of either UI generation."""
    text = pane_text()
    for marker in ("trust this folder", "Do you trust",
                   "Choose the text style", "to continue"):
        if marker in text:
            log(f"answering dialog: {marker!r}")
            tmux("send-keys", "-t", "=default:1", "Enter")
            return False
    return any(m in text for m in ("? for shortcuts", "⏵⏵", "╭"))


def main():
    PROJ.mkdir(parents=True)
    (PROJ / "README.md").write_text("integration-test scratch project\n")
    DATA.mkdir()
    CFG.write_text(json.dumps({"socket": SOCK, "seed_session": "default",
                               "seed_window": "h"}))
    # Skip onboarding and the use-this-API-key dialog (probe-verified fields).
    (HOME / ".claude.json").write_text(json.dumps({
        "hasCompletedOnboarding": True,
        "theme": "dark",
        "customApiKeyResponses": {"approved": [DUMMY_KEY[-20:]], "rejected": []},
    }))

    port = fake_anthropic.serve(log_path=str(API_LOG))
    log(f"fake Anthropic API on 127.0.0.1:{port}")

    ver = subprocess.run(["claude", "--version"], capture_output=True, text=True)
    log(f"claude version: {ver.stdout.strip() or ver.stderr.strip()}")

    env_line = (f"HOME={HOME} ANTHROPIC_BASE_URL=http://127.0.0.1:{port} "
                f"ANTHROPIC_API_KEY={DUMMY_KEY} "
                f"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 "
                f"DISABLE_AUTOUPDATER=1 DISABLE_TELEMETRY=1 "
                f"PATH={os.environ['PATH']}")

    before = set(claude_pids())
    tmux("new-session", "-d", "-x", "200", "-y", "50", "-s", "default", "-n", "h",
         "tail -f /dev/null")
    tmux("new-window", "-d", "-t", "=default:1", "-n", "claudewin", "-c", str(PROJ),
         f"env {env_line} bash --norc --noprofile")
    tmux("send-keys", "-t", "=default:1", " claude", "Enter")

    try:
        wait_for("claude prompt (dialogs answered)", answer_dialogs)
        pid = wait_for("claude process",
                       lambda: next(iter(set(claude_pids()) - before), None))
        reg = HOME / ".claude" / "sessions" / f"{pid}.json"
        sid = wait_for("session registry entry",
                       lambda: (json.loads(reg.read_text()).get("sessionId")
                                if reg.exists() else None))
        log(f"claude pid={pid} sid={sid[:8]}…")

        # A conversation must EXIST for `claude --resume` to find it: a
        # never-messaged session has no transcript on disk and resume
        # correctly reports "No conversation found". Sending one message
        # also drives a real request into the fake API — whose canned
        # answer (or any answer) must not matter to suspend/resume.
        tmux("send-keys", "-t", "=default:1",
             "hello from the go-tmux-saver integration test", "Enter")
        transcript_glob = HOME / ".claude" / "projects"
        wait_for("transcript persisted",
                 lambda: list(transcript_glob.glob(f"*/{sid}.jsonl")))
        time.sleep(2)  # let the (mock) response round-trip settle

        # ── suspend ──
        r = gts("claude-suspend", "default", "1")
        log(f"claude-suspend rc={r.returncode}\n{r.stdout}{r.stderr}")
        if r.returncode != 0 or "suspended 1, failed 0" not in r.stdout:
            sys.exit("claude-suspend did not report success")
        wait_for("claude process gone", lambda: pid not in claude_pids())
        wait_for("placeholder banner", lambda: "Enter = resume" in pane_text())
        if sid[:8] not in pane_text():
            sys.exit(f"banner does not show the session id\n{pane_text()}")
        captures = list((DATA / "suspend").glob("*.txt"))
        if not captures:
            sys.exit("no saved-output capture written")
        log(f"capture: {captures[0].name} ({captures[0].stat().st_size} bytes)")

        # ── the parked pane saves back as the same session ──
        r = gts("save", "--no-display")
        log(f"save rc={r.returncode} {r.stdout.strip()}")
        layout = json.loads(next(DATA.glob("snap-*/layout.json")).read_text())
        kinds = [p["restore"] for s in layout["sessions"]
                 for w in s["windows"] for p in w["panes"]]
        if {"kind": "claude", "claude_session": sid} not in kinds:
            sys.exit(f"snapshot lost the claude pane: {kinds}")
        log("snapshot round-trip: claude pane with same session id")

        # ── resume: Enter relaunches claude ──
        before2 = set(claude_pids())
        tmux("send-keys", "-t", "=default:1", "Enter")
        pid2 = wait_for("claude relaunched",
                        lambda: next(iter(set(claude_pids()) - before2), None))

        # Readiness must look at the BOTTOM of the pane: the placeholder's
        # old banner (with its own prompt markers) is still on screen until
        # claude's TUI redraws, so a whole-pane match returns instantly and
        # a /exit typed then lands in a still-loading session.
        def resumed_prompt_ready():
            tail = "\n".join(pane_text().split("\n")[-8:])
            return any(m in tail for m in ("❯", "⏵⏵", "? for shortcuts"))
        wait_for("claude prompt after resume", resumed_prompt_ready)
        time.sleep(3)  # let the restored conversation finish settling
        log(f"resumed: new claude pid={pid2}")

        # ── suspend again (idempotent parking) and finish ──
        for attempt in (1, 2):
            r = gts("claude-suspend", "default", "1")
            if r.returncode == 0 and "suspended 1, failed 0" in r.stdout:
                break
            log(f"re-suspend attempt {attempt} failed:\n{r.stdout}{r.stderr}")
            if attempt == 2:
                sys.exit("re-suspend failed twice")
            time.sleep(5)
        wait_for("claude gone again", lambda: pid2 not in claude_pids())

        # ── the API must not have mattered ──
        reqs = API_LOG.read_text() if API_LOG.exists() else ""
        if reqs:
            log(f"NOTE: claude made API request(s) — functionality above "
                f"passed regardless:\n{reqs}")
        else:
            log("fake API received ZERO requests — suspend/resume has no "
                "API dependency")
        log("PASS: full suspend → save → resume → suspend cycle "
            "(no credentials, no real API)")
    finally:
        tmux("kill-server", check=False)


if __name__ == "__main__":
    main()
