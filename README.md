# go-tmux-saver

`go-tmux-saver` snapshots and restores tmux sessions — windows, panes, layouts,
working directories, running commands, and (optionally) pane scrollback — over
a single tmux control-mode connection, rather than shelling out to `tmux` once
per pane the way tmux-resurrect does. It is a from-scratch replacement for the
tmux-resurrect + tmux-continuum stack, built after that stack failed silently
on ten64 in several independent ways (see the design doc). Snapshots are
written atomically to a per-host data directory, with a guard against
degenerate saves, retention/pruning, and drift-checked systemd `--user` units
for periodic saving, on-start restore, staleness watching, and failure
alerting.

## Install / build

```sh
CGO_ENABLED=0 go build -ldflags "-s -w -X github.com/mithro/go-tmux-saver/internal/cli.Version=$(git describe --tags)" -o go-tmux-saver ./cmd/go-tmux-saver
```

Requires Go 1.26+. The binary is self-contained (`CGO_ENABLED=0`); copy it
anywhere on `$PATH`.

## Usage

Every subcommand accepts `--config <path>` (default: the XDG config path),
plus `--socket`/`--data-dir` overrides where applicable. Run
`go-tmux-saver <command> -h` for the full flag list.

- **`save`** — snapshot the running tmux server.
  ```sh
  go-tmux-saver save --auto --no-display
  ```
  `--auto` makes "no tmux server" a clean `skipped` exit (for timer units);
  `--no-display` suppresses the in-tmux `display-message` summary. Prints
  `kept panes=N …` (new snapshot written) or `unchanged panes=N …` (nothing
  changed since the last save).

- **`restore`** — graft a saved snapshot onto the running server.
  ```sh
  go-tmux-saver restore --on-start   # only if the server is seed-only; else "skipped: server not seed-only"
  go-tmux-saver restore --merge      # default: restore additively into whatever is live now
  ```

- **`status`** — last save time, recent events, timer state, data dir.
  ```sh
  go-tmux-saver status
  go-tmux-saver status --check-fresh   # exit 1 if the last good save is stale
  go-tmux-saver status --json          # machine-readable, for the watchdog/alerting units
  ```

- **`prune`** — remove snapshots outside the configured retention policy.
  ```sh
  go-tmux-saver prune --dry-run
  ```

- **`setup`** — manage the systemd `--user` units and tmux.conf snippet.
  ```sh
  go-tmux-saver setup generate --dir ./out   # render the managed files without touching the system
  go-tmux-saver setup install                # install units + tmux.conf snippet, enable timers
  go-tmux-saver setup validate                # report drift between installed files and what generate would produce
  go-tmux-saver setup update                  # re-render and apply drift, restarting affected units
  ```

- **`alert`** — send a rate-limited sendmail alert for a failed/recovered unit (invoked by the generated `OnFailure=` unit, not normally run by hand).
  ```sh
  go-tmux-saver alert --unit go-tmux-saver.service
  go-tmux-saver alert --unit go-tmux-saver.service --recovered
  ```

- **`import-resurrect`** — one-time conversion of an existing tmux-resurrect save into a go-tmux-saver snapshot.
  ```sh
  go-tmux-saver import-resurrect ~/.tmux/resurrect/tmux_resurrect_*.txt --contents ~/.tmux/resurrect/pane_contents.tar.gz
  ```

- **`version`** — print the build version.

## How it works

See the design spec at
[`docs/superpowers/specs/2026-08-22-go-tmux-saver-design.md`](docs/superpowers/specs/2026-08-22-go-tmux-saver-design.md)
for the full architecture (save engine, restore/merge rules, systemd unit
topology, failure handling) and
[`docs/superpowers/plans/2026-08-22-go-tmux-saver-core.md`](docs/superpowers/plans/2026-08-22-go-tmux-saver-core.md)
for the task-by-task implementation plan this repo was built from.

## Status

This is Plan 1 (core tool) — save/restore/status/prune/setup/alert/import are
all implemented and tested. Packaging and fleet rollout (a signed apt repo,
the Ansible role that deploys and enables it, and retiring the old
tmux-resurrect/tmux-continuum rcfiles config) is **Plan 2**, not yet started.
