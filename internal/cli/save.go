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
	"sync"
	"syscall"
	"time"

	"github.com/mithro/go-tmux-saver/internal/collect"
	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/mail"
	"github.com/mithro/go-tmux-saver/internal/procs"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
	"github.com/mithro/go-tmux-saver/internal/trace"
)

// alertUnit and watchAlertUnit are the systemd unit names
// go-tmux-saver-alert@.service is instantiated with (see the Task 16
// templates), and thus the RateLimiter keys a success must clear.
//
// RULING R46: a successful save clears BOTH. Nothing else ever cleared the
// watch unit's marker — `status --check-fresh` didn't, and the watch unit
// only ever runs the alert on FAILURE — so after the first staleness mail
// the watchdog stayed rate-limited forever and never mailed again. A save
// succeeding is exactly the condition the watch unit tests for, so it ends
// that streak too.
const (
	alertUnit      = "go-tmux-saver.service"
	watchAlertUnit = "go-tmux-saver-watch.service"
)

// alertUnits is the full set of units whose alert markers a success clears.
var alertUnits = []string{alertUnit, watchAlertUnit}

// clearAlertsAndNotify clears each unit's rate-limit marker under dataDir
// and sends exactly one recovery mail for every marker that actually
// existed. Sending is best-effort: a sendmail failure is returned for the
// caller to log, never turned into a non-zero exit — the operation that
// triggered the recovery already succeeded.
func clearAlertsAndNotify(dataDir, host, mailTo, body string, units []string) []error {
	rl := mail.RateLimiter{Dir: dataDir}
	var errs []error
	for _, u := range units {
		if !rl.Clear(u) {
			continue
		}
		if err := mail.Send(mail.Sendmail, mailTo, mail.Subject(host, u, true), body); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", u, err))
		}
	}
	return errs
}

// lockFile is the exclusive save lock inside the data dir (RULING R47).
const lockFile = ".lock"

// tryLockDataDir takes a NON-BLOCKING exclusive flock on <dir>/.lock. ok is
// false (with a nil error) when another process already holds it — the
// caller should skip rather than queue: these are periodic saves, and the
// next timer tick is a better time to try than the tail of a save that is
// still running.
//
// flock is per open file description, not per process, so this contends
// correctly even against another save inside the same process (which is
// how the tests drive it). The lock is released when release() runs or,
// failing that, when the process exits and the fd is closed — so a crashed
// save never leaves the store permanently locked.
func tryLockDataDir(dir string) (release func(), ok bool, err error) {
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			f.Close()
		})
	}, true, nil
}

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
	// Warn reports a non-fatal problem that must not be swallowed (a
	// failed events.log append, an un-touchable fresh marker). nil logs to
	// stderr.
	Warn func(msg string)
}

// warn reports a non-fatal problem, defaulting to stderr when the caller
// supplied no Warn hook.
func (d SaveDeps) warn(msg string) {
	if d.Warn != nil {
		d.Warn(msg)
		return
	}
	fmt.Fprintln(os.Stderr, "go-tmux-saver: warning:", msg)
}

// Outcome describes what one RunSave call did.
type Outcome struct {
	// Kind is one of: kept | unchanged | rejected-degenerate | skipped |
	// error. "skipped" means another save held the data dir's lock
	// (RULING R47) — nothing was collected and nothing is wrong.
	Kind             string
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
		if err := snapshot.AppendEvent(d.Store.Dir, e); err != nil {
			d.warn("events.log: " + err.Error())
		}
	}

	// RULING R47: one save at a time per data dir. Two concurrent saves
	// would race over Stage/Promote, the `last` symlink and pruning; the
	// loser skips rather than waits.
	release, locked, err := tryLockDataDir(d.Store.Dir)
	if err != nil {
		logEv("error", nil, "", err.Error())
		return Outcome{Kind: "error"}, err
	}
	if !locked {
		logEv("skipped", nil, "", "save in progress")
		return Outcome{Kind: "skipped", Duration: time.Since(start)}, nil
	}
	defer release()

	// Safe only under the lock: any snap-*.tmp still on disk now cannot
	// belong to a live save, because a live save holds this lock for its
	// whole staging window. (EnsureDir used to do this sweep on every
	// subcommand's bring-up, unlocked, where it could delete the staging
	// directory of a save that was still writing into it.)
	if err := d.Store.CleanStaleTmp(); err != nil {
		d.warn("stale staging sweep: " + err.Error())
	}

	c := &collect.Collector{T: d.T, Procs: d.Procs, Reg: d.Reg, Allowlist: d.Cfg.Allowlist, Host: d.Host}
	stop := trace.Time("save.collect")
	snap, contents, warnings, err := c.Collect(ctx)
	stop()
	if err != nil {
		logEv("error", nil, "", err.Error())
		return Outcome{Kind: "error"}, err
	}
	// RULING R48: non-fatal collection problems (a pane that vanished
	// mid-save, so its capture-pane answered with a %error) ride along in
	// the event detail rather than being dropped on the floor.
	warnDetail := ""
	if len(warnings) > 0 {
		warnDetail = "warn: " + strings.Join(warnings, "; ")
	}
	if !d.Cfg.Contents.Enabled {
		contents = map[string][]byte{}
	}
	newPanes, _ := snap.CountPanes()

	stop = trace.Time("save.last")
	last, _, lerr := d.Store.Last()
	stop()
	lastPanes := 0
	if lerr == nil {
		lastPanes, _ = last.CountPanes()
		stop = trace.Time("save.unchanged")
		same := collect.Unchanged(last, snap)
		stop()
		if same {
			if err := snapshot.TouchFresh(d.Store.Dir); err != nil {
				d.warn("fresh marker: " + err.Error())
			}
			logEv("unchanged", snap, "", warnDetail)
			d.Display(fmt.Sprintf("unchanged (%d panes)", newPanes))
			return Outcome{Kind: "unchanged", Panes: newPanes, LastPanes: lastPanes, Duration: time.Since(start)}, nil
		}
	}

	stop = trace.Time("save.stage")
	stg, err := d.Store.Stage(snap, contents)
	stop()
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

	stop = trace.Time("save.promote")
	dir, err := stg.Promote()
	stop()
	if err != nil {
		stg.Discard()
		logEv("error", snap, "", err.Error())
		return Outcome{Kind: "error"}, err
	}
	if err := snapshot.TouchFresh(d.Store.Dir); err != nil {
		d.warn("fresh marker: " + err.Error())
	}
	logEv("kept", snap, filepath.Base(dir), warnDetail)
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

		stop := trace.Time("cmd.dial")
		tr, err := openTransport(ctx, cfg)
		stop()
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

		stop = trace.Time("cmd.procs-scan")
		tb, err := procs.Scan("/proc")
		stop()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		host, err := os.Hostname()
		if err != nil {
			host = "unknown-host"
		}
		home, _ := os.UserHomeDir()

		stop = trace.Time("cmd.list-clients")
		clients := countClients(ctx, tr)
		stop()

		d := SaveDeps{
			T: tr, Store: store, Procs: tb,
			Reg:     procs.ClaudeRegistry{Dir: filepath.Join(home, ".claude", "sessions")},
			Cfg:     cfg,
			Host:    host,
			Clients: clients,
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
		if o.Kind == "skipped" {
			// RULING R47: another save holds the data dir's lock. Say so
			// even for a manual save (the user pressed M-s and deserves to
			// know why nothing happened), and exit 0 — nothing is wrong.
			fmt.Fprintln(stdout, "skipped: save in progress")
			return 0
		}

		summary := fmt.Sprintf("%s panes=%d last=%d %s", o.Kind, o.Panes, o.LastPanes, o.Duration.Round(time.Millisecond))
		fmt.Fprintln(stdout, summary)

		if *auto && (o.Kind == "kept" || o.Kind == "unchanged") {
			for _, err := range clearAlertsAndNotify(store.Dir, host, cfg.MailTo, "save succeeded: "+summary, alertUnits) {
				fmt.Fprintln(stderr, "alert: recovery mail:", err)
			}
		}
		return 0
	}})
}
