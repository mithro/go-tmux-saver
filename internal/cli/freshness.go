package cli

import (
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

// freshTier is how stale the last good save is, relative to the configured
// warn/stale thresholds.
type freshTier int

const (
	tierFresh  freshTier = iota // within warn_stale_factor × interval
	tierWarn                    // past warn, within stale
	tierStale                   // past watch_stale_factor × interval (the email watchdog's limit)
	tierNoSave                  // no good save on record at all
)

// classifyFreshness reads the last-good-save marker and buckets its age into
// a tier. It is intentionally cheap (a single stat of the fresh marker, no
// tmux connection): the tmux status line runs it every status-interval.
func classifyFreshness(dataDir string, cfg config.Config, now time.Time) (freshTier, time.Duration) {
	lastGood, ok, _ := snapshot.LastGood(dataDir)
	if !ok {
		return tierNoSave, 0
	}
	age := now.Sub(lastGood)
	warn := time.Duration(cfg.IntervalMinutes*cfg.WarnStaleFactor) * time.Minute
	stale := time.Duration(cfg.IntervalMinutes*cfg.WatchStaleFactor) * time.Minute
	switch {
	case age >= stale:
		return tierStale, age
	case age >= warn:
		return tierWarn, age
	default:
		return tierFresh, age
	}
}

// compactAge renders a duration as a short, status-line-friendly string:
// minutes under an hour, hours under a day, then days.
func compactAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// freshnessTmux returns the tmux status-line fragment for the current save
// freshness. It is empty when saves are fresh — so the segment is invisible
// in the common case — and empty when the indicator is disabled in config,
// so the status line can carry the fragment unconditionally and let config
// (not a tmux.conf regeneration) turn it on and off. When stale it emits a
// coloured, self-terminating (#[default]) warning sized to the age.
func freshnessTmux(dataDir string, cfg config.Config, now time.Time) string {
	if !cfg.StatusIndicator {
		return ""
	}
	tier, age := classifyFreshness(dataDir, cfg, now)
	switch tier {
	case tierNoSave:
		return "#[fg=red,bold] ⚠ no save#[default]"
	case tierStale:
		return fmt.Sprintf("#[fg=red,bold] ⚠ save %s#[default]", compactAge(age))
	case tierWarn:
		return fmt.Sprintf("#[fg=yellow] ⚠ save %s#[default]", compactAge(age))
	default:
		return ""
	}
}

// freshnessPlain returns a human-readable one-liner for a `freshness` run
// with no --tmux flag (a quick CLI health check, distinct from the fuller
// `status` output).
func freshnessPlain(dataDir string, cfg config.Config, now time.Time) string {
	tier, age := classifyFreshness(dataDir, cfg, now)
	switch tier {
	case tierNoSave:
		return "no save"
	case tierStale:
		return "stale " + compactAge(age)
	case tierWarn:
		return "warn " + compactAge(age)
	default:
		return "fresh " + compactAge(age)
	}
}

func init() {
	register(command{"freshness", "print a stale-save indicator (a tmux status-line token with --tmux)", func(args []string, stdout, stderr io.Writer) int {
		fs := flag.NewFlagSet("freshness", flag.ContinueOnError)
		asTmux := fs.Bool("tmux", false, "emit a tmux status-line format fragment (empty when saves are fresh or the indicator is disabled)")
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

		now := time.Now()
		if *asTmux {
			// No trailing newline: the value is spliced into status-right.
			fmt.Fprint(stdout, freshnessTmux(store.Dir, cfg, now))
		} else {
			fmt.Fprintln(stdout, freshnessPlain(store.Dir, cfg, now))
		}
		return 0
	}})
}
