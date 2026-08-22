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

func init() {
	register(command{"restore", "graft a saved snapshot onto the running tmux server", func(args []string, stdout, stderr io.Writer) int {
		fs := flag.NewFlagSet("restore", flag.ContinueOnError)
		onStart := fs.Bool("on-start", false, "only restore if the server is seed-only (nothing but the seed session/window)")
		merge := fs.Bool("merge", false, "restore additively against whatever is live now (default)")
		snapshotDir := fs.String("snapshot", "", "restore from this snapshot directory instead of the store's last")
		noContents := fs.Bool("no-contents", false, "skip pane scrollback replay even if contents are enabled")
		socket := fs.String("socket", "", "override config socket")
		dataDir := fs.String("data-dir", "", "override config data dir")
		cfgPath := fs.String("config", config.Path(), "config file")
		fs.SetOutput(stderr)
		if err := fs.Parse(args); err != nil {
			return 2
		}
		_ = merge // --merge is the default behaviour; the flag exists for explicitness/documentation

		cfg, err := config.Load(*cfgPath)
		if err == nil {
			if *socket != "" {
				cfg.Socket = *socket
			}
			if *dataDir != "" {
				cfg.DataDir = *dataDir
			}
			err = cfg.Validate()
		}
		if err != nil {
			fmt.Fprintln(stderr, "config:", err)
			return 2
		}

		codec, ok := snapshot.LookupCodec(cfg.Contents.Codec)
		if !ok {
			fmt.Fprintf(stderr, "config: unknown codec %q\n", cfg.Contents.Codec)
			return 2
		}
		store := &snapshot.Store{Dir: cfg.DataDir, Codec: codec}
		if err := store.EnsureDir(); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		tr, err := openTransport(ctx, cfg)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer tr.Close()

		live, err := restore.QueryLive(ctx, tr)
		if err != nil {
			fmt.Fprintln(stderr, "restore:", err)
			return 1
		}

		if *onStart && !restore.IsSeedOnly(live, cfg.SeedSession, cfg.SeedWindow) {
			fmt.Fprintln(stdout, "skipped: server not seed-only")
			return 0
		}

		var snap *snapshot.Snapshot
		var lastDir string
		if *snapshotDir != "" {
			lastDir = *snapshotDir
			snap, err = store.Load(lastDir)
		} else {
			snap, lastDir, err = store.Last()
		}
		if err != nil {
			fmt.Fprintln(stderr, "restore: no snapshot to restore from:", err)
			return 1
		}

		paneByKey := map[string]snapshot.Pane{}
		for _, se := range snap.Sessions {
			for _, w := range se.Windows {
				for _, p := range w.Panes {
					paneByKey[snapshot.PaneKey(se.Name, w.Index, p.Index)] = p
				}
			}
		}
		contentsEnabled := cfg.Contents.Enabled && !*noContents
		contentsFn := func(paneKey string) ([]byte, bool) {
			p, ok := paneByKey[paneKey]
			if !ok {
				return nil, false
			}
			data, err := store.ReadContent(lastDir, p)
			if err != nil {
				return nil, false
			}
			return data, true
		}

		opts := restore.Options{
			ClaudeResumePath: expandHome(cfg.ClaudeResumePath),
			Contents:         contentsEnabled,
			SeedSession:      cfg.SeedSession,
			SeedWindow:       cfg.SeedWindow,
		}
		plan := restore.BuildPlan(live, snap, opts)

		start := time.Now()
		report, err := restore.Apply(ctx, tr, plan, contentsFn)
		if err != nil {
			fmt.Fprintln(stderr, "restore:", err)
			return 1
		}
		for _, n := range report.Notes {
			fmt.Fprintln(stderr, "note:", n)
		}

		windows := report.Created + report.Relocated + report.Skipped
		detail := fmt.Sprintf("sessions=%d windows=%d relocated=%d skipped=%d", len(snap.Sessions), windows, report.Relocated, report.Skipped)
		snapshot.AppendEvent(store.Dir, snapshot.Event{
			Time: time.Now(), Outcome: "restore",
			Sessions: len(snap.Sessions), Windows: windows, Clients: live.Clients,
			DurationMS: time.Since(start).Milliseconds(), Detail: detail,
		})

		fmt.Fprintf(stdout, "restored %d sessions, %d windows (%d relocated, %d skipped)\n", len(snap.Sessions), windows, report.Relocated, report.Skipped)
		return 0
	}})
}
