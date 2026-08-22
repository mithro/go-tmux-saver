package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mithro/go-tmux-saver/internal/collect"
	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/procs"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

// SaveDeps bundles everything RunSave needs so it can be driven by real tmux
// state (the "save" subcommand below) or by a tmuxctl.Fake (tests).
type SaveDeps struct {
	T       tmuxctl.Transport
	Store   *snapshot.Store
	Procs   *procs.Table
	Reg     procs.ClaudeRegistry
	Cfg     config.Config
	Host    string
	Clients int
	Display func(msg string)
}

// Outcome describes what one RunSave call did.
type Outcome struct {
	Kind             string // kept | unchanged | rejected-degenerate | skipped | error
	Dir              string
	Panes, LastPanes int
	Duration         time.Duration
}

// RunSave collects the live tmux state via d.T and stores it, deduplicating
// unchanged saves and guarding against degenerate (accidental-clobber)
// snapshots. Every outcome — including errors — is recorded to the store's
// events log.
func RunSave(ctx context.Context, d SaveDeps) (Outcome, error) {
	start := time.Now()
	logEv := func(kind string, snap *snapshot.Snapshot, file, detail string) {
		e := snapshot.Event{Time: time.Now(), Outcome: kind, Clients: d.Clients, DurationMS: time.Since(start).Milliseconds(), File: file, Detail: detail}
		if snap != nil {
			e.Panes, e.Windows = snap.CountPanes()
			e.Sessions = len(snap.Sessions)
		}
		snapshot.AppendEvent(d.Store.Dir, e)
	}

	c := &collect.Collector{T: d.T, Procs: d.Procs, Reg: d.Reg, Allowlist: d.Cfg.Allowlist, Host: d.Host}
	snap, contents, err := c.Collect(ctx)
	if err != nil {
		logEv("error", nil, "", err.Error())
		return Outcome{Kind: "error"}, err
	}
	if !d.Cfg.Contents.Enabled {
		contents = map[string][]byte{}
	}
	newPanes, _ := snap.CountPanes()

	last, _, lerr := d.Store.Last()
	lastPanes := 0
	if lerr == nil {
		lastPanes, _ = last.CountPanes()
		if collect.Unchanged(last, snap) {
			snapshot.TouchFresh(d.Store.Dir)
			logEv("unchanged", snap, "", "")
			d.Display(fmt.Sprintf("unchanged (%d panes)", newPanes))
			return Outcome{Kind: "unchanged", Panes: newPanes, LastPanes: lastPanes, Duration: time.Since(start)}, nil
		}
	}

	stg, err := d.Store.Stage(snap, contents)
	if err != nil {
		logEv("error", snap, "", err.Error())
		return Outcome{Kind: "error"}, err
	}

	if lerr == nil && snapshot.IsDegenerate(newPanes, lastPanes, d.Cfg.Guard.MinPanes, d.Cfg.Guard.Divisor) {
		dir, err := stg.Reject()
		if err != nil {
			logEv("error", snap, "", err.Error())
			return Outcome{Kind: "error"}, err
		}
		detail := fmt.Sprintf("%d vs %d", newPanes, lastPanes)
		logEv("rejected-degenerate", snap, filepath.Base(dir), detail)
		d.Display("rejected: degenerate (" + detail + ")")
		return Outcome{Kind: "rejected-degenerate", Dir: dir, Panes: newPanes, LastPanes: lastPanes, Duration: time.Since(start)}, nil
	}

	dir, err := stg.Promote()
	if err != nil {
		stg.Discard()
		logEv("error", snap, "", err.Error())
		return Outcome{Kind: "error"}, err
	}
	snapshot.TouchFresh(d.Store.Dir)
	logEv("kept", snap, filepath.Base(dir), "")
	if _, perr := snapshot.Prune(d.Store.Dir, d.Cfg.Retention.Keep, d.Cfg.Retention.DailyDays, d.Cfg.Retention.Rejected, time.Now()); perr != nil {
		// Pruning is best-effort housekeeping: a failure here doesn't
		// invalidate the save that just succeeded, but must not be silently
		// dropped either.
		logEv("error", snap, filepath.Base(dir), "prune: "+perr.Error())
	}
	d.Display(fmt.Sprintf("saved %d panes in %.1fs", newPanes, time.Since(start).Seconds()))
	return Outcome{Kind: "kept", Dir: dir, Panes: newPanes, LastPanes: lastPanes, Duration: time.Since(start)}, nil
}

func init() {
	register(command{"save", "snapshot the running tmux server", func(args []string, stdout, stderr io.Writer) int {
		fs := flag.NewFlagSet("save", flag.ContinueOnError)
		auto := fs.Bool("auto", false, "timer mode: no-server is 'skipped' and exits 0")
		noDisplay := fs.Bool("no-display", false, "do not display-message a summary in tmux")
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
			if *auto && isNoServer(err) {
				snapshot.AppendEvent(store.Dir, snapshot.Event{Time: time.Now(), Outcome: "skipped", Detail: "no server"})
				fmt.Fprintln(stdout, "skipped: no tmux server")
				return 0
			}
			// Any other Dial failure (including isNoServer without --auto) is
			// a genuine error, not a skip: log it and exit 1 either way.
			snapshot.AppendEvent(store.Dir, snapshot.Event{Time: time.Now(), Outcome: "error", Detail: err.Error()})
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer tr.Close()

		tb, err := procs.Scan("/proc")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		host, _ := os.Hostname()
		home, _ := os.UserHomeDir()

		d := SaveDeps{
			T: tr, Store: store, Procs: tb,
			Reg:     procs.ClaudeRegistry{Dir: filepath.Join(home, ".claude", "sessions")},
			Cfg:     cfg,
			Host:    host,
			Clients: countClients(ctx, tr),
			Display: func(string) {},
		}
		if !*noDisplay {
			d.Display = func(m string) { tr.Run(ctx, fmt.Sprintf("display-message %q", "go-tmux-saver: "+m)) }
		}

		o, err := RunSave(ctx, d)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s panes=%d last=%d %s\n", o.Kind, o.Panes, o.LastPanes, o.Duration.Round(time.Millisecond))
		return 0
	}})
}
