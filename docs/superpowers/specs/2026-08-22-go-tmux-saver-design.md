# go-tmux-saver — design

*Status: design approved in brainstorming 2026-08-22; implementation plan to follow.*

## 1. Why

The current tmux persistence stack on the fleet (tmux-resurrect + tmux-continuum +
three local hook scripts) failed on ten64 on 2026-08-20 in four independent ways
during one reboot:

1. **Silent autosave death.** continuum's autosave trigger is a `#()` interpolation
   prepended to `status-right`. A routine `source-file` on 2026-07-28 reset
   `status-right`; continuum only re-adds its hook when it is the *sole* tmux
   server, so it never came back. Nothing noticed for 23 days. A session group
   created in that window was unrecoverable.
2. **No auto-restore** (deliberately unset on ten64; only one host had it) and a
   fragile gate when it *is* set: continuum restores only if the server's
   `#{start_time}` is within a few minutes of *now* — ten64 boots with a clock
   skewed by weeks until GPS/NTP steps it, which can race that check.
3. **Restore into a live server migrated a saved window name onto a running
   pane** (resurrect never overwrites an existing window index but does adopt
   its name), and the save had captured junk grouped clones (`default-N`).
4. **Guard gap.** The local degenerate-save guard protected the layout file
   but not `pane_contents.tar.gz`, which a 1-pane post-boot save clobbered.

Separately, **saves are slow**: resurrect's `save.sh` forks ~6 processes per
pane (including a full `ps -ao` scan per pane) and one `tmux` client per
query; at 46 panes on ten64 a save takes ~10 s and blocks the UI when run
manually. ~100 % of that time is fork/IPC overhead, not data.

## 2. Goals and non-goals

**Goals**

- Never lose state silently: saves happen on a timer independent of any
  client being attached; failures and staleness are detected and mailed.
- Deterministic restore on server start, no clock heuristics.
- Additive, conflict-safe restore into a live server.
- Fast: < 0.5 s for 50 panes.
- Fleet-wide: stock tmux of any version from 3.5a upward (laptop, ten64,
  desktop.buddy, big-storage, any future host); no dependency on the
  `mithro/tmux` fork.
- The binary owns its own configuration lifecycle (generate / install /
  validate / update) so ansible can converge hosts with check→correct→verify.
- Preserve: the Claude confirm-before-resume placeholder UX
  (`~/bin/claude-resume <uuid>`), the `M-s`/`M-r` keybindings, and the
  degenerate-save guard + audit-log semantics.

**Non-goals**

- Reading/writing tmux-resurrect's on-disk format as a living format (a
  one-time `import-resurrect` converter only).
- Native save/restore commands inside tmux itself (possible later
  optimisation; the tool may detect and prefer them if they appear).
- Cross-host/remote snapshots, encryption, or multi-user servers.

## 3. Architecture

One static Go binary, **`go-tmux-saver`**, with subcommands `save`, `restore`,
`status`, `prune`, `import-resurrect`, `setup {generate,install,validate,
update}`, `alert`.

| Piece | Responsibility |
|---|---|
| `go-tmux-saver save` | One control-mode connection + one `/proc` pass → snapshot directory (`layout.json` + per-pane hardlinked scrollback files); guard; event log; prune. |
| `go-tmux-saver restore` | Plan + apply an additive merge of a snapshot into the running server; `--on-start` mode for seed-only servers. |
| `go-tmux-saver.timer/.service` (user) | Periodic `save --auto`, `Persistent=true`, runs attached or detached. |
| `go-tmux-saver-watch.timer/.service` (user) | Hourly `status --check-fresh`; mails if the newest good save is older than 3× the interval. |
| `go-tmux-saver-alert@.service` (user) | `OnFailure=` target; `go-tmux-saver alert` mails the failure via `sendmail -t`. |
| `tmux-server.service.d/50-go-tmux-saver.conf` (drop-in) | `ExecStartPost=go-tmux-saver restore --on-start` on the existing, unmodified unit. |
| `~/.config/go-tmux-saver/tmux.conf` | Generated keybinding snippet (`M-s`, `M-r`) sourced from the rcfiles tmux config by one guarded line. |
| `~/.config/go-tmux-saver/config.json` | Generated defaults; the only file a human edits. |
| `~/.local/share/go-tmux-saver/` | `snap-<UTC>/` directories (`layout.json` + `panes/` one file per pane, hardlinked when unchanged), `last` symlink, `rejected/`, `events.log`, freshness marker. |

Everything the tool needs from tmux comes over **a single control-mode
connection per invocation** (`tmux -L main -C …`). Everything it needs about
processes comes from **one pass over `/proc`** plus Claude's per-pid registry
(`~/.claude/sessions/<pid>.json`). There are no per-pane forks anywhere.

The tmux config side shrinks: resurrect, continuum and the three hook scripts
(`resurrect-post-save`, `claude-resume-save-hook`, `continuum-attach-grace`)
are retired. `~/bin/claude-resume` stays as the placeholder the restore types
into Claude panes.

## 4. Save engine

**Collection.** Over the control-mode connection, in order:
`list-sessions`, `list-windows -a`, `list-panes -a` with tab-delimited `-F`
formats (`session_name`, `session_group`, `session_grouped`, `window_index`,
`window_id`, `window_name`, `window_layout`, `window_active`, `window_flags`,
`pane_index`, `pane_id`, `pane_pid`, `pane_active`, `pane_current_path`,
`pane_current_command`, `pane_title`, `history_size`), the per-window
`automatic-rename` option, then `capture-pane -epJ -S -<history_size> -t <id>`
for every pane. Replies are parsed from the `%begin/%end/%error` framing.
Grouped clones (`#{session_grouped}` = 1) are skipped; only base sessions
persist. Per-command timeouts; `%exit` or server death aborts with `error`.

**Process resolution.** One `/proc` scan builds pid → {ppid, comm, cmdline,
starttime}. Per pane, BFS the subtree from `pane_pid` (shallowest first).
Claude panes resolve via the registry file for the claude pid, validated by
`procStart` against `/proc/<pid>/stat` (stale-pid safe) and cross-checked
against the registry's own `tmux` pane field; fallback: a `--resume <uuid>` /
`claude-resume <uuid>` argv match. For other panes the foreground command is
recorded as a restore argv only if its comm is on the configurable allowlist
(default: `ssh mosh-client claude claude-resume vi vim nvim emacs man less more
tail top htop`); otherwise the pane restores as a plain shell in its cwd.

**Snapshot format.** A snapshot is a **directory** `snap-<UTC timestamp>/`
holding `layout.json` (`"schema": 1`, small — structure only) and `panes/`
(one compressed scrollback file per pane, see below):

```
{ schema, host, tmux_version, taken_at, server_start, contents_codec,
  sessions: [ { name, active_window,
      windows: [ { index, name, layout, active, flags, automatic_rename,
          panes: [ { index, id, cwd, title, active, history_lines,
                     content_sha256, content_file,
                     restore: { kind: "shell"|"argv"|"claude", argv: [...],
                                claude_session: "<uuid>" } } ] } ] } ],
  client: { session },                # last attached client's session
  stats: { panes, windows, sessions, duration_ms } }
```

Restore commands are argv arrays (no shell quoting).

**Per-pane files and hardlinks.** Each pane's captured scrollback is written
to `panes/<session>_<window>_<pane>.txt<codec-ext>` (structural name, not the
`%N` pane id — ids are reused after a server restart) and `layout.json`
records `content_sha256` (hash of the *uncompressed* capture) and the
`content_file` name. If the hash equals the hash of the structurally-same pane
in the previous snapshot, the new file is a **hardlink** to that previous
file — nothing is written. Idle placeholders and quiet shells therefore cost
zero bytes per save. Retention is a plain `rm -r snap-<old>/`: the
filesystem's link count is the garbage collector, so there is no manifest GC
and no dangling reference. Hardlinks require one filesystem, which the single
data dir guarantees. `import-resurrect` unpacks resurrect's
`pane_contents.tar.gz` into the same per-pane layout.

**Compression is pluggable.** A `Codec` interface (`Name`, `Ext`, `NewWriter`,
`NewReader`) with a registry; `gzip` (stdlib) is the default and initially the
only codec. Each per-pane file carries the codec's extension and `layout.json`
records `contents_codec`; restore selects the decoder from the snapshot, never
from a default, so adding zstd later (e.g. `klauspost/compress`) is a
registration plus a config switch, and old snapshots stay readable. Because
the hash is of the uncompressed capture, switching codec does not defeat
hardlink dedup across the switch (the file is simply rewritten once).

**Atomicity.** The snapshot is built in `snap-<UTC>.tmp/` (files written,
fsync'd, hardlinks created), then the directory is renamed into place and the
`last` symlink swapped atomically. A crash mid-save leaves at most a `*.tmp/`
directory (removed on the next run) — never a partial snapshot or a dangling
`last`.

**Guard and promotion.** Same rule as today: a save is *degenerate* when
`last` is rich (≥ `guard.min_panes`, default 5) and the new pane count × `guard.divisor` (default 3)
≤ `last`'s pane count. Degenerate snapshot directories are moved under
`rejected/` (pruned) and **nothing is promoted — layout and scrollback
together** — this closes the `pane_contents.tar.gz` hole. A snapshot
structurally identical to `last` is logged `unchanged` and not kept, but a
freshness marker is touched so the watchdog sees a healthy save. "Identical"
ignores timestamps, `history_lines`, content hashes, and *shell* pane titles
(they churn with every `cd`), but **includes titles beginning with `✳`** —
Claude Code sets those to its conversation summary, which is meaningful state.
Titles are always saved regardless.

**Event log.** `events.log`, one tab-separated line per attempt:
`iso_ts  outcome(kept|unchanged|rejected-degenerate|skipped|error)  panes
windows  sessions  clients  duration_ms  file  [detail]`.

**Retention.** `prune` (run after each `--auto` save) keeps the newest 50
snapshots plus one per day for 30 days; `rejected/` keeps 20.

**Performance budget.** < 0.5 s for 50 panes; the only O(panes) work is the
in-socket capture replies.

## 5. Trigger, restore and lifecycle integration

**Periodic save.** `go-tmux-saver.timer`: `OnBootSec=2min`,
`OnUnitActiveSec=10min`, `Persistent=true`, `AccuracySec=1min`.
`go-tmux-saver.service`: `Type=oneshot`, `After=tmux-server.service`,
`ExecStart=go-tmux-saver save --auto`, `OnFailure=go-tmux-saver-alert@%n.service`.
`--auto` exit codes: 0 for `kept|unchanged|rejected-degenerate|skipped` (all
logged; `skipped` = no server running, normal right after boot), non-zero only
for hard errors (tmux reachable but a command/IO failure, write failure).

**Watchdog.** `go-tmux-saver-watch.timer` hourly → `status --check-fresh`:
non-zero (→ alert mail) when the newest `kept|unchanged` is older than 3× the
configured interval. This is the detector the 23-day blackout lacked.

**Restore on start.** The drop-in adds
`ExecStartPost=go-tmux-saver restore --on-start` after the existing
`remain-on-exit` line of `tmux-server.service`. `--on-start` proceeds only when
the server is *seed-only* (exactly the seed session `default` with the single
seed window); otherwise it logs `skipped: server not seed-only` and exits 0.
No time-based heuristic. zprofile's login clone only needs `default` to exist,
which it does before `ExecStartPost`, so logins during a restore simply see
windows appear.

**Merge semantics** (`restore`, `restore --merge`, `M-r`) — strictly additive:

- Missing sessions are created exactly as saved (name, windows, indices,
  layout, active window/pane).
- Within an existing session, **existing windows are never renamed, moved or
  modified.**
- A saved window whose index is free is created there; whose index is occupied
  by the *same* name is logged `skipped`; if a window with the saved *name*
  already exists anywhere in the session (at another index — e.g. an earlier
  relocation) it is also `skipped` (`present at index N`), which keeps re-runs
  idempotent; otherwise (index occupied by a *different* name, name absent) it
  is created at the next free index and logged `relocated`.
- Panes: split to the saved count, then `select-layout <saved layout>`; each
  pane starts the default shell with `-c <cwd>`; the restore argv (or
  `~/bin/claude-resume <uuid>` for Claude panes) is typed via `send-keys` so a
  shell remains when it exits. Panes whose cwd no longer exists fall back to
  `$HOME` (logged).
- Scrollback replay (`--contents`, default on, config-switchable): the decoded
  scrollback is written to `<data dir>/replay/<run-id>/<pane key>.txt` (0600;
  older replay dirs are removed at the start of each restore) and a single
  ` cat '<path>'` command is typed into the fresh pane before its restore
  command, so the saved text is *displayed*. Saved content is never pasted or
  fed to the shell as keystrokes (`paste-buffer` would execute every line).
- Restore commands are typed as ONE shell-quoted `send-keys` argument (tmux
  concatenates multiple `send-keys` arguments with no separator).
- The last attached client's session is selected; grouped clones are never
  restored (login machinery recreates them).
- Idempotent: re-running completes a partial restore; the plan and every
  create/relocate/skip is logged.

**Manual keys.** `M-s` → `run-shell "go-tmux-saver save"`,
`M-r` → `run-shell "go-tmux-saver restore --merge"`; both report one line via
`display-message` (`saved 46 panes in 0.3s`, `rejected: degenerate (1 vs 46)`,
`restored 3 sessions, 2 windows relocated`).

## 6. Configuration lifecycle (`setup`)

All managed files are rendered from templates embedded in the binary and carry
a header `# managed by go-tmux-saver <version> — edit config.json, not this
file; run 'go-tmux-saver setup update'`. Unmanaged files are never touched.

- `setup generate [--dir DIR]` — render every managed file (user units, the
  `tmux-server.service.d/50-go-tmux-saver.conf` drop-in, the tmux keybinding
  snippet, `config.json` defaults) to stdout or a directory.
- `setup install` — write them atomically to the real locations
  (`$XDG_CONFIG_HOME/systemd/user/`, `$XDG_CONFIG_HOME/go-tmux-saver/`),
  `systemctl --user daemon-reload`, enable + start the timers. Idempotent.
- `setup validate [--json]` — check: managed files present and byte-identical
  to the expected rendering (drift diff), `systemd-analyze --user verify` on
  the units, timers enabled + active, the drop-in visible in
  `systemctl --user show tmux-server.service -p ExecStartPost`, the running
  tmux has `M-s`/`M-r` bound to the tool, the data dir exists with mode 0700,
  control mode reachable. Non-zero exit with a drift list.
- `setup update [--dry-run]` — re-render with the running binary's templates,
  show the diff, apply, `daemon-reload`, restart timers.

`config.json` keys (with defaults): `socket` (`main`), `interval_minutes` (10),
`watch_stale_factor` (3), `allowlist` (list above), `guard.min_panes` (5),
`guard.divisor` (3), `contents.enabled` (true), `contents.codec` (`gzip`),
`retention` (`{keep: 50, daily_days: 30, rejected: 20}`), `mail_to`
(`$USER`), `claude_resume_path` (`~/bin/claude-resume`).

The only thing outside the tool is one guarded line in the rcfiles tmux
config (`if-shell "test -r ~/.config/go-tmux-saver/tmux.conf" "source-file
~/.config/go-tmux-saver/tmux.conf"`), replacing the resurrect/continuum block.
`setup validate` is what an ansible `tmux_saver` role runs as
check → correct (`setup install` / `update`) → verify. Per-user installs are
run by ansible (or by hand on the laptop); the Debian package's postinst does
nothing user-level.

## 7. Repository, distribution and rollout

**Repository** — `github.com/mithro/go-tmux-saver`, Apache-2.0, Go module
`github.com/mithro/go-tmux-saver`, `CGO_ENABLED=0` static builds. Configured
per the github-setup standards: `v0.0` tag on the first commit, merge commits
only, wiki/projects/discussions disabled, branch protection (no force-push /
deletion) on `main`, secret scanning + push protection, auto-delete head
branches, always-suggest-update-branch, tag ruleset enforcing `vXX.ZZZ`.

**Distribution** — GitHub Actions: `go test` → cross-build arm64/amd64 →
`.deb` (nfpm) → signed apt repository published at
`mithro.github.io/go-tmux-saver` with a per-repo signing key, mirrored through
`apt-proxy.welland.mithis.com/go-tmux-saver` (same pipeline shape as
`mithro/tmux`). Version from `git describe`.

**Rollout per host** (order: ten64 → desktop.buddy → big-storage → laptop):

1. install the package;
2. `go-tmux-saver import-resurrect ~/.local/share/tmux/resurrect/last` so the
   first restore has data;
3. `go-tmux-saver setup install` (timers run alongside continuum briefly);
4. confirm `status` shows `kept` saves and the watchdog is green;
5. land the rcfiles change that retires resurrect/continuum/hook scripts and
   sources the keybinding snippet; `setup validate` passes.

## 8. Testing

- The control-mode transport is an interface; unit tests use a fake backed by
  **recorded transcripts** from real servers (tmux next-3.8 and 3.5a) as golden
  files, so parsing is tested against both versions without tmux in the loop.
- Table-tested pure cores: guard decision (ported from
  `resurrect-post-save.is_degenerate`), `/proc` tree resolution on fixture
  trees incl. stale-pid registry entries, Claude registry cross-checks, the
  **merge planner** (live state + snapshot → command list) with explicit cases
  for: existing window at a saved index must never be renamed, relocate on
  conflict, same-name skip, seed-only detection, missing cwd fallback; codec
  registry round-trip; snapshot schema round-trip; retention pruning;
  event-log parsing and `status`.
- Storage layer: hash → hardlink decisions (unchanged pane links to the
  previous snapshot's file, changed pane writes a new one, structural naming
  survives pane-id reuse after a server restart), atomic `*.tmp/` → rename
  promotion, retention `rm -r` leaving shared files alive (link counts),
  codec switch rewriting once without breaking dedup, `unchanged` comparison
  honouring the shell-title / `✳`-title rule.
- CI integration tests against a real tmux on a throwaway socket
  (`tmux -L gts-test-$$`): build layout → `save` → `kill-server` → start seed →
  `restore --on-start` → assert identical structure; plus the merge path into a
  live server. `setup` is tested in a temp `$HOME`/`$XDG_*` with an injectable
  `systemctl` path.
- TDD throughout; no feature without its failing test first.

## 9. Failure handling and security

- Loud and never half-written: temp+fsync+rename everywhere; every outcome is
  in `events.log`, on stderr, and in the exit code.
- Alerts via `sendmail -t` through the fleet mail relay, rate-limited to one
  mail per failure streak plus one on recovery.
- `status [--json]` exposes the last N events, freshness, timer state.
- Data dir `0700`, files `0600` (snapshots contain scrollback).
- Restore is idempotent; partial restores are completed by re-running.

## 10. Decisions log

| Decision | Choice | Why |
|---|---|---|
| Engine | Custom compiled tool, not a resurrect fork | fork storm is architectural; hooks already half a custom system |
| Language | Go | existing Go momentum, trivial static cross-compile, testability |
| Where it runs | Standalone binary, stock tmux | fleet-wide incl. 3.5a and laptop; fork-only features ruled out |
| Trigger | systemd user timer | independent of attached clients and `status-right` |
| Restore gate | seed-only server check | deterministic; immune to boot clock skew |
| Format | New versioned JSON + one-time importer | user chose not to preserve resurrect's format |
| Compression | pluggable codec interface, gzip default | zstd later without format break |
| Storage layout | snapshot directory; one compressed file per pane; hardlink when content hash unchanged | idle panes cost nothing; filesystem link count = GC; no tarball to clobber |
| Pane titles | always saved; shell titles excluded from the `unchanged` compare, `✳` Claude titles included | Claude titles are conversation summaries (state); shell titles churn |
| Config ownership | the binary (`setup`) | user requirement; enables ansible check→correct→verify |
| Repo standards | github-setup skill, `vXX.ZZZ` tags | user's shared GitHub.md |
