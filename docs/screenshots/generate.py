#!/usr/bin/env python3
# /// script
# requires-python = ">=3.11"
# dependencies = ["rich>=13"]
# ///
"""Regenerate docs/images/*.svg — the Claude-integration screenshots.

Everything on screen is SYNTHETIC: a fake $HOME, a fabricated session
transcript for a made-up project, and a stub `claude` binary that draws a
Claude-ish prompt box and exits on /exit. No real Claude sessions,
transcripts, or credentials are read, so nothing can leak.

Run from the repo root (needs tmux and uv):

    uv run docs/screenshots/generate.py
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
IMAGES = REPO / "docs" / "images"
SOCK = "gts-docs-shots"
SID = "3f2a9c81-5b47-4e2e-9d10-6c8f0b2a7e55"  # fabricated

DEMO_OUTPUT = """\
$ go test ./resolver/...
ok      dns-tool/resolver      0.41s
$ git status --short
 M resolver/retry.go
"""


def sh(*args, **kw):
    return subprocess.run(args, check=True, capture_output=True, text=True, **kw)


def tmux(*args, check=True):
    return subprocess.run(["tmux", "-L", SOCK, "-f", "/dev/null", *args],
                          check=check, capture_output=True, text=True)


def build_binary(tmp: Path) -> Path:
    out = tmp / "go-tmux-saver"
    sh("go", "build", "-o", str(out), "./cmd/go-tmux-saver", cwd=str(REPO))
    return out


def fake_home(tmp: Path) -> Path:
    """A synthetic $HOME: one fabricated transcript for ~/code/dns-tool."""
    home = tmp / "home"
    launch = home / "code" / "dns-tool"
    launch.mkdir(parents=True)
    munged = str(launch).replace("/", "-").replace(".", "-")
    proj = home / ".claude" / "projects" / munged
    proj.mkdir(parents=True)
    lines = [
        {"type": "user", "cwd": str(launch), "gitBranch": "retry-logic",
         "timestamp": "2026-08-27T09:12:44Z",
         "message": {"content": [{"type": "text",
                                  "text": "make the resolver retry with backoff"}]}},
        {"type": "summary", "summary": "Resolver retry with exponential backoff"},
        {"type": "assistant", "cwd": str(launch),
         "timestamp": "2026-08-27T09:41:02Z"},
    ]
    (proj / f"{SID}.jsonl").write_text("\n".join(json.dumps(l) for l in lines) + "\n")
    (home / ".claude" / "sessions").mkdir(parents=True)
    (home / "bin").mkdir()
    return home


def fake_claude(tmp: Path, home: Path) -> Path:
    """A stub `claude` that draws a prompt box, registers itself in the
    session registry (like the real thing), and exits on /exit."""
    path = tmp / "bin" / "claude"
    path.parent.mkdir(parents=True, exist_ok=True)
    # Direct interpreter shebang on purpose: with `env` in between, the
    # process comm becomes "python3" and the suspend detection (comm ==
    # "claude") would not see it; a direct shebang keeps comm = "claude".
    path.write_text(f'''#!/usr/bin/python3
import json, os, sys
pid = os.getpid()
start = open(f"/proc/{{pid}}/stat").read().split()[21]
reg = os.path.join({str(home)!r}, ".claude", "sessions", f"{{pid}}.json")
open(reg, "w").write(json.dumps(
    {{"pid": pid, "sessionId": {SID!r}, "procStart": start}}))
print("\\033[38;5;208m*\\033[0m Claude Code \\033[2m(demo stub)\\033[0m")
print()
print("\\033[2m>\\033[0m resolver retries now use exponential backoff with jitter;")
print("  all 14 tests pass. Anything else before I commit?")
print()
print("\\u256d" + "\\u2500" * 60 + "\\u256e")
print("\\u2502 > " + " " * 57 + "\\u2502")
print("\\u2570" + "\\u2500" * 60 + "\\u256f")
for line in sys.stdin:
    if line.strip() == "/exit":
        break
''')
    path.chmod(0o755)
    return path


def capture(target: str, scrollback: int = 0) -> str:
    r = tmux("capture-pane", "-epJ", "-S", f"-{scrollback}", "-t", target)
    return r.stdout.rstrip("\n")


def render_svg(ansi: str, title: str, out: Path, width=78):
    from rich.console import Console
    from rich.text import Text
    lines = []
    blanks = 0
    for l in ansi.split("\n"):
        blanks = blanks + 1 if not l.strip() else 0
        if blanks <= 1:
            lines.append(l)
    while lines and not lines[-1].strip():
        lines.pop()
    console = Console(record=True, width=width, force_terminal=True)
    console.print(Text.from_ansi("\n".join(lines)), highlight=False)
    console.save_svg(str(out), title=title)
    print(f"wrote {out.relative_to(REPO)}")


def main():
    IMAGES.mkdir(parents=True, exist_ok=True)
    tmp = Path(tempfile.mkdtemp(prefix="gts-shots-"))
    try:
        binary = build_binary(tmp)
        home = fake_home(tmp)
        claude = fake_claude(tmp, home)
        saved = tmp / "saved.txt"
        saved.write_text(DEMO_OUTPUT)
        data_dir = tmp / "store"
        cfg = tmp / "config.json"
        cfg.write_text(json.dumps({"socket": SOCK, "seed_session": "default",
                                   "seed_window": "h"}))
        env_prefix = f"HOME={home} PATH={claude.parent}:/usr/bin:/bin"

        tmux("new-session", "-d", "-s", "default", "-n", "h",
             "tail -f /dev/null")
        tmux("new-window", "-d", "-t", "=default:1", "-n", "resolver")

        # ── Shot 1: the placeholder as a restored pane shows it ──
        tmux("send-keys", "-t", "=default:1",
             f" clear; {env_prefix} {binary} claude-resume {SID} "
             f"--saved-output {saved}", "Enter")
        time.sleep(1.0)
        render_svg(capture("=default:1"),
                   "restored Claude pane — go-tmux-saver claude-resume",
                   IMAGES / "claude-resume-banner.svg")
        tmux("send-keys", "-t", "=default:1", "C-c")

        # ── Shot 2: claude-suspend parking a live session ──
        # The pane runs a bare --norc bash whose env carries the fake
        # HOME/PATH: the stub claude AND the placeholder claude-suspend later
        # types both resolve against the synthetic transcript, and the shot
        # shows a generic "bash-…$" prompt instead of the real user@host
        # (tmux -e is defeated by rc files resetting PATH/prompt).
        tmux("new-window", "-d", "-t", "=default:2", "-n", "claudewin",
             f"env HOME={home} PATH={claude.parent}:/usr/bin:/bin "
             f"bash --norc --noprofile")
        tmux("send-keys", "-t", "=default:2", " clear; claude", "Enter")
        time.sleep(1.0)
        r = subprocess.run([str(binary), "claude-suspend", "--socket", SOCK,
                            "--config", str(cfg), "--data-dir", str(data_dir),
                            "default", "2"],
                           capture_output=True, text=True,
                           env={**os.environ, "HOME": str(home)})
        time.sleep(1.0)
        cmdline = (f"$ claude-suspend claudewin\n{r.stdout}"
                   + (r.stderr or ""))
        render_svg(cmdline + "\n" + "─" * 60 + "\n" + capture("=default:2", scrollback=25),
                   "claude-suspend — /exit, confirm, park behind the placeholder",
                   IMAGES / "claude-suspend.svg")

        # ── Shot 3: what a save sees — the placeholder round-trips ──
        r = subprocess.run([str(binary), "save", "--socket", SOCK,
                            "--config", str(cfg), "--data-dir", str(data_dir),
                            "--no-display"],
                           capture_output=True, text=True,
                           env={**os.environ, "HOME": str(home)})
        layouts = list(data_dir.glob("snap-*/layout.json"))
        pane = "?"
        if layouts:
            snap = json.loads(layouts[0].read_text())
            for s in snap["sessions"]:
                for w in s["windows"]:
                    for p in w["panes"]:
                        if p["restore"]["kind"] == "claude":
                            pane = json.dumps(p["restore"], indent=2)
        text = (f"$ go-tmux-saver save\n{r.stdout}"
                f"$ jq '.. | select(.kind? == \"claude\")' layout.json\n{pane}\n")
        render_svg(text, "the suspended pane saves back as the same Claude session",
                   IMAGES / "claude-roundtrip.svg")
    finally:
        tmux("kill-server", check=False)
        shutil.rmtree(tmp)


if __name__ == "__main__":
    main()
