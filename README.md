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

Set `GTS_TRACE=1` to print per-phase timings (dial, `/proc` scan, each
collection command, the `capture-pane` loop, staging) to stderr — a save's
wall time is dominated by tmux's own `capture-pane` cost, and this shows the
split. Off by default; normal output is unchanged.

- **`save`** — snapshot the running tmux server.
  ```sh
  go-tmux-saver save --auto --no-display
  ```
  `--auto` makes "no tmux server" a clean `skipped` exit (for timer units);
  `--no-display` suppresses the in-tmux `display-message` summary. Prints
  `kept panes=N …` (new snapshot written) or `unchanged panes=N …` (nothing
  changed since the last save). Only one save runs against a data directory
  at a time: if another already holds the lock this one prints
  `skipped: save in progress` and exits 0.

- **`restore`** — graft a saved snapshot onto the running server.
  ```sh
  go-tmux-saver restore --on-start   # only if the server is seed-only; else "skipped: server not seed-only"
  go-tmux-saver restore --merge      # default: restore additively into whatever is live now
  ```

- **`status`** — last save time, recent events, timer state, data dir.
  ```sh
  go-tmux-saver status
  go-tmux-saver status --check-fresh   # exit 1 if the last good save is stale
  go-tmux-saver status --json          # machine-readable (same data, no exit-code change)
  ```
  `--check-fresh` is what the watch unit runs, in text mode: stale ⇒ a
  `STALE: …` line and exit 1 (which fires the alert unit); fresh ⇒ exit 0,
  and the watch unit's alert marker is cleared so the next failure streak
  mails again. `--json` only changes the output format — it can be combined
  with `--check-fresh`.

- **`prune`** — remove snapshots outside the configured retention policy.
  ```sh
  go-tmux-saver prune --dry-run
  ```

- **`setup`** — manage the systemd `--user` units and tmux.conf snippet.
  ```sh
  go-tmux-saver setup generate               # print the managed files to stdout (=== <path> === separators)
  go-tmux-saver setup generate --dir ./out   # ...or render them into a directory, still touching nothing else
  go-tmux-saver setup install                # install units + tmux.conf snippet, enable timers
  go-tmux-saver setup validate               # report drift between installed files and what generate would produce
  go-tmux-saver setup validate --json        # same, as {"ok":bool,"drifts":[{"path","kind","diff"}]}
  go-tmux-saver setup update                 # re-render and apply drift, restarting affected units
  ```
  An existing `config.json` is never overwritten by `install`, `generate
  --dir` or `update` — it is yours to edit; delete it to get a fresh
  default. `validate`/`update` exit 1 when drift was found.

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

## Data directory layout

Everything lives under `$XDG_DATA_HOME/go-tmux-saver` (default
`~/.local/share/go-tmux-saver`, mode `0700`), overridable per-command with
`--data-dir`:

| Path | What it is |
|------|------------|
| `snap-<UTC timestamp>/layout.json` | one snapshot: sessions, windows, panes, layouts, cwds, restore commands |
| `snap-<UTC timestamp>/panes/<session>_<win>_<pane>.txt.gz` | that snapshot's pane scrollback, one file per pane (hardlinked to the previous snapshot when unchanged) |
| `snap-<UTC timestamp>.tmp/` | a save still staging; swept by the next save that holds the lock |
| `last` | symlink to the newest good snapshot — what `restore` reads |
| `rejected/snap-<…>/` | snapshots the degenerate-save guard refused, kept for inspection |
| `events.log` | append-only, tab-separated: time, outcome, counts, duration, file, detail |
| `fresh` | empty marker; its mtime is the last good save, and `status --check-fresh` compares against it |
| `alert-<unit>.service` | rate-limit marker: one alert per failure streak, cleared on recovery |
| `replay/<run-id>/<pane>.txt` | scrollback a restore `cat`s back into panes; swept by the next restore |
| `.lock` | flock held for the duration of a save, so two saves never race |

## config.json keys

`$XDG_CONFIG_HOME/go-tmux-saver/config.json` (mode `0600`). Every key is
optional — anything absent falls back to the built-in default that `setup
generate` prints.

| Key | Default | Meaning |
|-----|---------|---------|
| `socket` | `main` | tmux socket name (`tmux -L <socket>`) |
| `seed_session` / `seed_window` | `default` / `h` | the always-present shell; `restore --on-start` only acts on a server holding nothing but this, and it is never touched by a restore |
| `interval_minutes` | `10` | the save timer's period (rendered into the timer unit) |
| `watch_stale_factor` | `3` | staleness limit = `interval_minutes × this` |
| `allowlist` | see `procs.DefaultAllowlist` | process names a pane's command may be relaunched from; anything else restores as a plain shell |
| `guard.min_panes` | `5` | below this pane count the degenerate-save guard doesn't engage |
| `guard.divisor` | `3` | reject a save with fewer than `last ÷ divisor` panes |
| `contents.enabled` | `true` | capture and replay pane scrollback |
| `contents.codec` | `gzip` | compression for pane content files |
| `retention.keep` | `50` | recent snapshots always kept |
| `retention.daily_days` | `30` | days over which one snapshot per day is kept beyond `keep` |
| `retention.rejected` | `20` | rejected snapshots kept |
| `mail_to` | `$USER` | recipient for failure/recovery alerts (via `sendmail -t`) |
| `claude_resume_path` | `~/bin/claude-resume` | helper used to relaunch a Claude session in a restored pane |

The data directory is derived, not configurable in the file — use
`--data-dir` or `$XDG_DATA_HOME`.

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
