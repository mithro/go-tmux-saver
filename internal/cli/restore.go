package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/restore"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

// expandHome expands a leading "~" in p to the user's home directory (no
// support for "~user" — only a bare leading "~").
func expandHome(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~/"))
		}
	}
	return p
}

// RestoreDeps bundles everything RunRestore needs, so it can be driven by
// real tmux state (the "restore" subcommand below) or a tmuxctl.Fake/live
// test server (tests) — mirrors SaveDeps/RunSave's shape.
type RestoreDeps struct {
	T           tmuxctl.Transport
	Store       *snapshot.Store
	Cfg         config.Config
	OnStart     bool
	SnapshotDir string // non-empty overrides Store.Last()
	NoContents  bool
	// PrepareReplayDir is called at most once, and only once RunRestore has
	// decided a restore will actually happen (i.e. AFTER the --on-start
	// seed-only check passes) — so an --on-start run that turns out to be a
	// skip never touches the replay directory at all. Its result is passed
	// to restore.Apply as the cat-replay directory.
	PrepareReplayDir func() (string, error)
}

// RestoreOutcome describes what one RunRestore call did.
type RestoreOutcome struct {
	Kind                                  string // restored | skipped-not-seed-only | skipped-no-snapshot
	Sessions, Windows, Relocated, Skipped int
	Errors                                int // planned creations that failed (see restore.Report.Notes)
	Notes                                 []string
	Duration                              time.Duration
}

// RunRestore queries the live server, decides (for --on-start) whether it's
// still seed-only, loads the snapshot to restore, builds the plan and
// applies it, logs a "restore" event, and returns a summary. It does not
// touch stdout/stderr — the "restore" subcommand below owns presentation.
func RunRestore(ctx context.Context, d RestoreDeps) (RestoreOutcome, error) {
	start := time.Now()

	live, err := restore.QueryLive(ctx, d.T)
	if err != nil {
		return RestoreOutcome{}, err
	}

	if d.OnStart && !restore.IsSeedOnly(live, d.Cfg.SeedSession, d.Cfg.SeedWindow) {
		return RestoreOutcome{Kind: "skipped-not-seed-only", Duration: time.Since(start)}, nil
	}

	// A restore will actually happen from here on — only now is it safe to
	// wipe/create the replay directory (minor: never touch it on an
	// --on-start skip).
	replayDir, err := d.PrepareReplayDir()
	if err != nil {
		return RestoreOutcome{}, fmt.Errorf("replay dir: %w", err)
	}

	var snap *snapshot.Snapshot
	var lastDir string
	var extraNotes []string
	if d.SnapshotDir != "" {
		lastDir = d.SnapshotDir
		snap, err = d.Store.Load(lastDir)
	} else {
		snap, lastDir, err = d.Store.Last()
		if errors.Is(err, snapshot.ErrDanglingLast) {
			// Issue #3: a dangling `last` is store corruption, not absence.
			// Recover from the newest intact snapshot — LOUDLY (an "error"
			// event plus a note on the outcome), never as a silent skip.
			danglingErr := err
			snapshot.AppendEvent(d.Store.Dir, snapshot.Event{
				Time: time.Now(), Outcome: "error", Clients: live.Clients,
				Detail: "restore: " + danglingErr.Error() + " — falling back to newest intact snapshot",
			})
			var ferr error
			snap, lastDir, ferr = d.Store.Newest()
			if ferr != nil {
				return RestoreOutcome{}, fmt.Errorf("%w; no intact snapshot to fall back to: %v", danglingErr, ferr)
			}
			extraNotes = append(extraNotes, fmt.Sprintf("dangling last symlink — fell back to %s", filepath.Base(lastDir)))
			err = nil
		}
	}
	if err != nil {
		// RULING R45: at boot the tmux-server.service drop-in runs
		// `restore --on-start` before any snapshot has ever been taken.
		// "there is nothing to restore from" is a normal state there, not a
		// failure, so it is logged as a skip and exits 0 — otherwise systemd
		// marks tmux-server.service failed on every first boot. Only the
		// genuinely-absent case (no `last` symlink) counts: a `last` that
		// exists but can't be read (corrupt layout.json, unreadable store)
		// is a hard failure even under --on-start.
		if d.OnStart && errors.Is(err, os.ErrNotExist) {
			snapshot.AppendEvent(d.Store.Dir, snapshot.Event{
				Time: time.Now(), Outcome: "skipped", Clients: live.Clients,
				DurationMS: time.Since(start).Milliseconds(), Detail: "no snapshot to restore",
			})
			return RestoreOutcome{Kind: "skipped-no-snapshot", Duration: time.Since(start)}, nil
		}
		return RestoreOutcome{}, fmt.Errorf("no snapshot to restore from: %w", err)
	}

	paneByKey := map[string]snapshot.Pane{}
	for _, se := range snap.Sessions {
		for _, w := range se.Windows {
			for _, p := range w.Panes {
				paneByKey[snapshot.PaneKey(se.Name, w.Index, p.Index)] = p
			}
		}
	}
	contentsEnabled := d.Cfg.Contents.Enabled && !d.NoContents
	var contentsFn func(paneKey string) ([]byte, bool)
	if contentsEnabled {
		// ContentReader parses lastDir's layout.json once (for its codec
		// name) instead of ReadContent's per-pane re-parse.
		read, err := d.Store.ContentReader(lastDir)
		if err != nil {
			return RestoreOutcome{}, fmt.Errorf("contents: %w", err)
		}
		contentsFn = func(paneKey string) ([]byte, bool) {
			p, ok := paneByKey[paneKey]
			if !ok {
				return nil, false
			}
			data, err := read(p)
			if err != nil {
				return nil, false
			}
			return data, true
		}
	} else {
		contentsFn = func(string) ([]byte, bool) { return nil, false }
	}

	opts := restore.Options{
		ClaudeResumePath: expandHome(d.Cfg.ClaudeResumePath),
		Contents:         contentsEnabled,
		SeedSession:      d.Cfg.SeedSession,
		SeedWindow:       d.Cfg.SeedWindow,
	}
	plan := restore.BuildPlan(live, snap, opts)

	report, err := restore.Apply(ctx, d.T, plan, contentsFn, replayDir)
	if err != nil {
		return RestoreOutcome{Notes: append(extraNotes, report.Notes...)}, err
	}

	windows := report.Created + report.Relocated + report.Skipped
	plannedWindows := plan.Created + plan.Relocated + plan.Skipped
	o := RestoreOutcome{
		Kind: "restored", Sessions: len(snap.Sessions), Windows: windows,
		Relocated: report.Relocated, Skipped: report.Skipped, Errors: plannedWindows - windows,
		Notes: append(extraNotes, report.Notes...), Duration: time.Since(start),
	}

	detail := fmt.Sprintf("sessions=%d windows=%d relocated=%d skipped=%d errors=%d", o.Sessions, o.Windows, o.Relocated, o.Skipped, o.Errors)
	// Panes comes from the snapshot that was restored (the Report counts
	// windows, not panes): a "restore" event with panes=0 was indistinguishable
	// from a restore that produced nothing.
	panes, _ := snap.CountPanes()
	snapshot.AppendEvent(d.Store.Dir, snapshot.Event{
		Time: time.Now(), Outcome: "restore",
		Panes: panes, Sessions: o.Sessions, Windows: o.Windows, Clients: live.Clients,
		DurationMS: o.Duration.Milliseconds(), Detail: detail,
	})

	return o, nil
}

// restoreSummary formats the one-line summary RunRestore's caller prints
// after a completed (possibly partially-failed) restore, and the process
// exit code that goes with it: any failed planned creation (o.Errors > 0)
// appends an "(N errors — see events.log)" suffix and calls for exit 1;
// otherwise exit 0. Split out from the "restore" subcommand's closure so
// this formatting/exit-code decision has its own direct unit test.
//
// RULING R45: under --on-start the suffix is still printed (and the error
// count still reaches events.log) but the exit code stays 0. That run is
// systemd's ExecStartPost= for tmux-server.service, and a partial restore
// must not fail the tmux server itself; exit 1 there is reserved for hard
// failures (unreadable store, transport error) that RunRestore returns as
// errors rather than as an Outcome.
func restoreSummary(o RestoreOutcome, onStart bool) (line string, exitCode int) {
	line = fmt.Sprintf("restored %d sessions, %d windows (%d relocated, %d skipped)", o.Sessions, o.Windows, o.Relocated, o.Skipped)
	if o.Errors > 0 {
		line += fmt.Sprintf(" (%d errors — see events.log)", o.Errors)
		if !onStart {
			exitCode = 1
		}
	}
	return line, exitCode
}

// prepareReplayDir removes any stale <dataDir>/replay/* directories left
// over from a previous restore, then creates a fresh
// <dataDir>/replay/<run-id>/ (run-id = UTC timestamp + this process's pid)
// for Apply's cat-replay files (RULING R26). The files are intentionally
// left in place after restore — cat runs asynchronously in the pane — and
// get swept on the NEXT restore.
//
// The wipe-then-create is NOT concurrency-safe: two "restore" processes
// racing against the same data dir could each glob the other's freshly
// created run-id directory and remove it out from under an in-flight cat.
// go-tmux-saver doesn't run concurrent restores against one data dir today
// (it's driven serially, e.g. by a systemd timer or --on-start at session
// start), so this is an accepted, documented limitation rather than a fix
// pending a real lock.
func prepareReplayDir(dataDir string) (string, error) {
	base := filepath.Join(dataDir, "replay")
	old, err := filepath.Glob(filepath.Join(base, "*"))
	if err != nil {
		return "", err
	}
	for _, d := range old {
		if err := os.RemoveAll(d); err != nil {
			return "", err
		}
	}
	runID := fmt.Sprintf("%s-%d", time.Now().UTC().Format("20060102T150405Z"), os.Getpid())
	dir := filepath.Join(base, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func init() {
	register(command{"restore", "graft a saved snapshot onto the running tmux server", func(args []string, stdout, stderr io.Writer) int {
		fs := flag.NewFlagSet("restore", flag.ContinueOnError)
		onStart := fs.Bool("on-start", false, "only restore if the server is seed-only (nothing but the seed session/window)")
		fs.Bool("merge", false, "restore additively against whatever is live now (default; this flag exists for explicitness/documentation)")
		snapshotDir := fs.String("snapshot", "", "restore from this snapshot directory instead of the store's last")
		noContents := fs.Bool("no-contents", false, "skip pane scrollback replay even if contents are enabled")
		socket := fs.String("socket", "", "override config socket")
		dataDir := fs.String("data-dir", "", "override config data dir")
		cfgPath := fs.String("config", config.Path(), "config file")
		fs.SetOutput(stderr)
		if err := fs.Parse(args); err != nil {
			return 2
		}

		cfg, store, msg, code := commonSetup(*cfgPath, *socket, *dataDir)
		if code != 0 {
			fmt.Fprintln(stderr, msg)
			return code
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		tr, err := openTransport(ctx, cfg)
		if err != nil {
			// RULING R29: on --on-start, a live server whose configured seed
			// session doesn't even exist is — by definition — not seed-only,
			// so treat it as the ordinary "nothing to do" skip rather than an
			// error. (No-server and every other Dial failure are unchanged.)
			if *onStart && isMissingSeedSession(err) {
				fmt.Fprintln(stdout, "skipped: server not seed-only")
				return 0
			}
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer tr.Close()

		o, err := RunRestore(ctx, RestoreDeps{
			T: tr, Store: store, Cfg: cfg,
			OnStart: *onStart, SnapshotDir: *snapshotDir, NoContents: *noContents,
			PrepareReplayDir: func() (string, error) { return prepareReplayDir(store.Dir) },
		})
		// Print whatever Notes RunRestore accumulated BEFORE reporting an
		// error — RunRestore can return a partial Notes list alongside a
		// non-nil err (e.g. restore.Apply failing outright on ctx
		// cancellation after some actions already ran), and those notes are
		// diagnostic context for the error, not something to withhold.
		for _, n := range o.Notes {
			fmt.Fprintln(stderr, "note:", n)
		}
		if err != nil {
			fmt.Fprintln(stderr, "restore:", err)
			return 1
		}
		if o.Kind == "skipped-not-seed-only" {
			fmt.Fprintln(stdout, "skipped: server not seed-only")
			return 0
		}
		if o.Kind == "skipped-no-snapshot" {
			fmt.Fprintln(stdout, "skipped: no snapshot to restore")
			return 0
		}

		summary, code := restoreSummary(o, *onStart)
		fmt.Fprintln(stdout, summary)
		return code
	}})
}
