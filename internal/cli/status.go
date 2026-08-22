package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

// timerState reports the go-tmux-saver systemd --user timer's active state.
// This "unknown" stub is overridden at package init by setup.go with the
// real `systemctl --user is-active go-tmux-saver.timer` check; it stays a
// package-level var (rather than a plain function) so tests can swap in a
// fake instead of shelling out to the real systemctl.
var timerState = func() string { return "unknown" }

// statusEvent is snapshot.Event's JSON shape for `status --json`: lower-snake
// keys, independent of Event's internal field names/order.
type statusEvent struct {
	Time       string `json:"time"`
	Outcome    string `json:"outcome"`
	Panes      int    `json:"panes"`
	Windows    int    `json:"windows"`
	Sessions   int    `json:"sessions"`
	Clients    int    `json:"clients"`
	DurationMS int64  `json:"duration_ms"`
	File       string `json:"file"`
	Detail     string `json:"detail"`
}

type statusReport struct {
	LastGood   string        `json:"last_good"`
	AgeSeconds int64         `json:"age_seconds"`
	Stale      bool          `json:"stale"`
	Events     []statusEvent `json:"events"`
	Snapshots  int           `json:"snapshots"`
}

// countSnapshots counts dataDir's snap-* subdirectories, excluding any
// still-staging snap-*.tmp ones.
func countSnapshots(dataDir string) int {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() && strings.HasPrefix(name, "snap-") && !strings.HasSuffix(name, ".tmp") {
			n++
		}
	}
	return n
}

// RunStatus prints go-tmux-saver's operational status to w: the last good
// save time and age, the last n events, the timer state, and the data dir
// with its snapshot count. Freshness (last good save missing, or older than
// interval_minutes × watch_stale_factor) is always computed — it drives the
// JSON "stale" field regardless of checkFresh — but only checkFresh turns
// staleness into a non-zero exit code (1) and, in text mode, an additional
// "STALE: ..." line.
func RunStatus(w io.Writer, dataDir string, cfg config.Config, asJSON, checkFresh bool, n int, now time.Time) int {
	lastGood, ok, _ := snapshot.LastGood(dataDir)
	limit := time.Duration(cfg.IntervalMinutes*cfg.WatchStaleFactor) * time.Minute
	var age time.Duration
	if ok {
		age = now.Sub(lastGood)
	}
	stale := !ok || age > limit

	events, _ := snapshot.TailEvents(dataDir, n)
	snapshots := countSnapshots(dataDir)

	if asJSON {
		je := make([]statusEvent, len(events))
		for i, e := range events {
			je[i] = statusEvent{
				Time: e.Time.UTC().Format(time.RFC3339), Outcome: e.Outcome,
				Panes: e.Panes, Windows: e.Windows, Sessions: e.Sessions, Clients: e.Clients,
				DurationMS: e.DurationMS, File: e.File, Detail: e.Detail,
			}
		}
		rep := statusReport{Stale: stale, Events: je, Snapshots: snapshots}
		if ok {
			rep.LastGood = lastGood.UTC().Format(time.RFC3339)
			rep.AgeSeconds = int64(age.Seconds())
		}
		b, _ := json.Marshal(rep)
		fmt.Fprintln(w, string(b))
	} else {
		if ok {
			fmt.Fprintf(w, "last good save: %s (%s ago)\n", lastGood.UTC().Format(time.RFC3339), age.Round(time.Second))
		} else {
			fmt.Fprintln(w, "last good save: never")
		}
		fmt.Fprintf(w, "timer: %s\n", timerState())
		fmt.Fprintf(w, "data dir: %s (%d snapshots)\n", dataDir, snapshots)
		fmt.Fprintln(w, "recent events:")
		for _, e := range events {
			fmt.Fprintf(w, "  %s %-20s panes=%d windows=%d sessions=%d clients=%d duration=%dms file=%s detail=%s\n",
				e.Time.UTC().Format(time.RFC3339), e.Outcome, e.Panes, e.Windows, e.Sessions, e.Clients, e.DurationMS, e.File, e.Detail)
		}
		if checkFresh && stale {
			ageStr := "never"
			if ok {
				ageStr = age.Round(time.Second).String() + " ago"
			}
			fmt.Fprintf(w, "STALE: last good save %s (limit %s)\n", ageStr, limit.Round(time.Second))
		}
	}

	if checkFresh && stale {
		return 1
	}
	return 0
}

func init() {
	register(command{"status", "show last save time, recent events, timer state, and data dir", func(args []string, stdout, stderr io.Writer) int {
		fs := flag.NewFlagSet("status", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "emit JSON instead of text")
		checkFresh := fs.Bool("check-fresh", false, "exit 1 if the last good save is missing or older than interval*watch_stale_factor")
		n := fs.Int("n", 10, "number of recent events to show")
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

		return RunStatus(stdout, store.Dir, cfg, *asJSON, *checkFresh, *n, time.Now())
	}})
}
