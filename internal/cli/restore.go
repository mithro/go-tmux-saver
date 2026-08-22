package cli

import (
	"context"
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
	ReplayDir   string // <dataDir>/replay/<run-id>; created+owned by the caller
}

// RestoreOutcome describes what one RunRestore call did.
type RestoreOutcome struct {
	Kind                                  string // restored | skipped-not-seed-only
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

	var snap *snapshot.Snapshot
	var lastDir string
	if d.SnapshotDir != "" {
		lastDir = d.SnapshotDir
		snap, err = d.Store.Load(lastDir)
	} else {
		snap, lastDir, err = d.Store.Last()
	}
	if err != nil {
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

	report, err := restore.Apply(ctx, d.T, plan, contentsFn, d.ReplayDir)
	if err != nil {
		return RestoreOutcome{Notes: report.Notes}, err
	}

	windows := report.Created + report.Relocated + report.Skipped
	plannedWindows := plan.Created + plan.Relocated + plan.Skipped
	o := RestoreOutcome{
		Kind: "restored", Sessions: len(snap.Sessions), Windows: windows,
		Relocated: report.Relocated, Skipped: report.Skipped, Errors: plannedWindows - windows,
		Notes: report.Notes, Duration: time.Since(start),
	}

	detail := fmt.Sprintf("sessions=%d windows=%d relocated=%d skipped=%d errors=%d", o.Sessions, o.Windows, o.Relocated, o.Skipped, o.Errors)
	snapshot.AppendEvent(d.Store.Dir, snapshot.Event{
		Time: time.Now(), Outcome: "restore",
		Sessions: o.Sessions, Windows: o.Windows, Clients: live.Clients,
		DurationMS: o.Duration.Milliseconds(), Detail: detail,
	})

	return o, nil
}

// prepareReplayDir removes any stale <dataDir>/replay/* directories left
// over from a previous restore, then creates a fresh
// <dataDir>/replay/<run-id>/ (run-id = UTC timestamp + this process's pid,
// so concurrent restores never collide) for Apply's cat-replay files
// (RULING R26). The files are intentionally left in place after restore —
// cat runs asynchronously in the pane — and get swept on the NEXT restore.
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

		replayDir, err := prepareReplayDir(store.Dir)
		if err != nil {
			fmt.Fprintln(stderr, "restore:", err)
			return 1
		}

		o, err := RunRestore(ctx, RestoreDeps{
			T: tr, Store: store, Cfg: cfg,
			OnStart: *onStart, SnapshotDir: *snapshotDir, NoContents: *noContents,
			ReplayDir: replayDir,
		})
		if err != nil {
			fmt.Fprintln(stderr, "restore:", err)
			return 1
		}
		for _, n := range o.Notes {
			fmt.Fprintln(stderr, "note:", n)
		}
		if o.Kind == "skipped-not-seed-only" {
			fmt.Fprintln(stdout, "skipped: server not seed-only")
			return 0
		}

		summary := fmt.Sprintf("restored %d sessions, %d windows (%d relocated, %d skipped)", o.Sessions, o.Windows, o.Relocated, o.Skipped)
		if o.Errors > 0 {
			summary += fmt.Sprintf(" (%d errors — see events.log)", o.Errors)
		}
		fmt.Fprintln(stdout, summary)
		if o.Errors > 0 {
			return 1
		}
		return 0
	}})
}
