#!/usr/bin/env python3
"""End-to-end integration test of claude-suspend / claude-resume against a
REAL Claude Code binary (issue #23).

Flow: start Claude in a tmux window → wait for its prompt (answering any
first-run trust/onboarding dialogs) → `go-tmux-saver claude-suspend` it →
assert the /exit-confirm-placeholder round-trip, the saved-output capture,
and that a `save` records the same session id → press Enter on the
placeholder → assert Claude relaunches → suspend again to leave everything
parked, then tear the server down.

Requirements: tmux, a `claude` binary on PATH, credentials in the
environment (CLAUDE_CODE_OAUTH_TOKEN or an already-authenticated
~/.claude), and GTS_BIN pointing at the go-tmux-saver build under test.
Stdlib only — CI runs it with a plain python3.
"""
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path

SOCK = os.environ.get("GTS_TEST_SOCKET", f"gts-ci-{os.getpid()}")
GTS = os.environ["GTS_BIN"]
HOME = Path(os.environ["HOME"])
DEADLINE = float(os.environ.get("GTS_TEST_TIMEOUT", "120"))

UUID_RE = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")


def log(msg):
    print(f"--- {msg}", flush=True)


def tmux(*args, check=True):
    r = subprocess.run(["tmux", "-L", SOCK, "-f", "/dev/null", *args],
                       capture_output=True, text=True)
    if check and r.returncode != 0:
        sys.exit(f"tmux {args} failed: {r.stderr}")
    return r


def gts(cmd, *rest):
    # Flags before positionals: the subcommands use stdlib flag parsing,
    # which stops at the first non-flag argument.
    return subprocess.run([GTS, cmd, "--socket", SOCK,
                           "--config", str(CFG), "--data-dir", str(DATA), *rest],
                          capture_output=True, text=True)


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
    """Live processes with comm 'claude' under this test's pane."""
    out = subprocess.run(["pgrep", "-x", "claude"], capture_output=True, text=True)
    return [int(p) for p in out.stdout.split()]


def answer_dialogs():
    """Answer Claude's first-run dialogs (trust folder / theme) with Enter
    until the input box appears. Returns True once the prompt is up."""
    text = pane_text()
    # Input-prompt markers across Claude UI generations: the boxed prompt
    # (╭), the bare chevron prompt (❯), and the shortcut/status hints.
    if any(m in text for m in ("╭", "❯", "? for shortcuts", "⏵⏵")):
        return True
    for marker in ("Do you trust", "Choose the text style", "to continue"):
        if marker in text:
            log(f"answering dialog: {marker!r}")
            tmux("send-keys", "-t", "=default:1", "Enter")
            break
    return False


def main():
    global CFG, DATA
    work = Path(os.environ.get("GTS_TEST_DIR", "/tmp")) / f"gts-ci-{os.getpid()}"
    proj = work / "demo-project"
    proj.mkdir(parents=True)
    (proj / "README.md").write_text("integration-test scratch project\n")
    DATA = work / "store"
    CFG = work / "config.json"
    CFG.write_text(json.dumps({"socket": SOCK, "seed_session": "default",
                               "seed_window": "h"}))

    ver = subprocess.run(["claude", "--version"], capture_output=True, text=True)
    log(f"claude version: {ver.stdout.strip() or ver.stderr.strip()}")

    before = set(claude_pids())
    tmux("new-session", "-d", "-x", "200", "-y", "50", "-s", "default", "-n", "h",
         "tail -f /dev/null")
    tmux("new-window", "-d", "-t", "=default:1", "-n", "claudewin", "-c", str(proj))
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

        # ── suspend ──
        r = gts("claude-suspend", "default", "1")
        log(f"claude-suspend rc={r.returncode}\n{r.stdout}{r.stderr}")
        if r.returncode != 0 or "suspended 1, failed 0" not in r.stdout:
            sys.exit("claude-suspend did not report success")
        wait_for("claude process gone", lambda: pid not in claude_pids())
        wait_for("placeholder banner",
                 lambda: "Enter = resume" in pane_text())
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
        log("PASS: full suspend → save → resume → suspend cycle")
    finally:
        tmux("kill-server", check=False)


if __name__ == "__main__":
    main()
