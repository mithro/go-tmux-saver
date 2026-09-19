#!/usr/bin/env python3
"""Run go-tmux-saver's test suite inside a resource-limited, tmux-isolated sandbox.

Use this on any host that also runs a *production* tmux server (ten64,
big-storage, a workstation). It:

  * relocates the tmux socket directory (TMUX_TMPDIR) into a private scratch
    dir and unsets TMUX, so no test can see or touch a real tmux server
    (defence in depth: the tests' own TestMain does this too);
  * caps CPU, memory and task count via a transient `systemd-run --user
    --scope` cgroup, so the suite cannot swamp the host;
  * cleans up afterwards (kills any leaked tmux servers, removes the scratch).

Anything after `--` (or any other args) is passed through to `go test`; with
no extra args it runs `go test ./...`.

Resource limits are overridable via the environment:
  GTS_TEST_MEM    (default 2G)     -> MemoryMax
  GTS_TEST_CPU    (default 300%)   -> CPUQuota  (300% = 3 cores)
  GTS_TEST_TASKS  (default 4096)   -> TasksMax

Stdlib only; no third-party dependencies.
"""

import glob
import os
import shutil
import subprocess
import sys
import tempfile


def go_test_argv(argv):
    """Build the `go test ...` command from passthrough args (after an optional --)."""
    if argv and argv[0] == "--":
        argv = argv[1:]
    return ["go", "test", *argv] if argv else ["go", "test", "./..."]


def user_manager_available():
    """True when a usable `systemd --user` manager is present for --user scopes."""
    if shutil.which("systemd-run") is None:
        return False
    proc = subprocess.run(
        ["systemctl", "--user", "is-system-running"],
        capture_output=True, text=True,
    )
    # "running" or "degraded" both mean the manager is up and can host a scope.
    state = proc.stdout.strip()
    return state in ("running", "degraded", "starting")


def kill_servers(tmux_tmpdir):
    """Best-effort kill of any tmux server whose socket lives under tmux_tmpdir."""
    for sock in glob.glob(os.path.join(tmux_tmpdir, "tmux-*", "*")):
        proc = subprocess.run(
            ["tmux", "-S", sock, "kill-server"], capture_output=True, text=True,
        )
        out = (proc.stdout + proc.stderr).strip()
        benign = ("no server running", "no such file or directory", "error connecting")
        if proc.returncode != 0 and not any(b in out for b in benign):
            print(f"sandboxed-test: kill-server {sock}: {out}", file=sys.stderr)


def main():
    cmd_go_test = go_test_argv(sys.argv[1:])

    mem = os.environ.get("GTS_TEST_MEM", "2G")
    cpu = os.environ.get("GTS_TEST_CPU", "300%")
    tasks = os.environ.get("GTS_TEST_TASKS", "4096")

    # The scratch dir becomes TMUX_TMPDIR, so tmux drops sockets at
    # <scratch>/tmux-$UID/<name>. Unix socket paths are capped at ~108 bytes,
    # so the base must be SHORT — a deep repo path overflows it. Use the
    # per-user runtime tmpfs ($XDG_RUNTIME_DIR, e.g. /run/user/1001): short,
    # per-user, made for sockets, and not /tmp. Fall back to the OS temp dir
    # (also short) when it is unavailable. TMPDIR is deliberately left alone
    # so tests that build their own sockets under t.TempDir() stay short too.
    runtime_base = os.environ.get("XDG_RUNTIME_DIR") or None
    scratch = tempfile.mkdtemp(prefix="gts-sbx-", dir=runtime_base)

    env = dict(os.environ)
    env.pop("TMUX", None)
    env["TMUX_TMPDIR"] = scratch

    if user_manager_available():
        cmd = [
            "systemd-run", "--user", "--scope", "--collect", "--quiet",
            "--same-dir",
            "-p", f"MemoryMax={mem}",
            "-p", "MemorySwapMax=0",
            "-p", f"CPUQuota={cpu}",
            "-p", f"TasksMax={tasks}",
            "--", *cmd_go_test,
        ]
        print(
            f"sandboxed-test: MemoryMax={mem} CPUQuota={cpu} TasksMax={tasks}, "
            f"TMUX_TMPDIR={scratch}",
            file=sys.stderr,
        )
    else:
        print(
            "sandboxed-test: systemd --user unavailable; running WITHOUT cgroup "
            "limits (tmux isolation still applied)",
            file=sys.stderr,
        )
        cmd = cmd_go_test

    try:
        rc = subprocess.call(cmd, env=env)
    finally:
        kill_servers(scratch)
        shutil.rmtree(scratch, ignore_errors=True)
    return rc


if __name__ == "__main__":
    sys.exit(main())
