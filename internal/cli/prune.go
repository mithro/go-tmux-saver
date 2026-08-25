package cli

import (
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

// RunPrune runs snapshot.Prune (or, with dryRun, its read-only counterpart
// snapshot.PruneCandidates) against dataDir using cfg's retention policy, and
// prints the names removed (or, in dry-run, the names that would be
// removed) to w.
func RunPrune(w io.Writer, dataDir string, cfg config.Config, dryRun bool, now time.Time) int {
	var removed []string
	var err error
	if dryRun {
		removed = snapshot.PruneCandidates(dataDir, cfg.Retention.Keep, cfg.Retention.DailyDays, cfg.Retention.Rejected, now)
	} else {
		removed, err = snapshot.Prune(dataDir, cfg.Retention.Keep, cfg.Retention.DailyDays, cfg.Retention.Rejected, now)
	}
	if err != nil {
		fmt.Fprintln(w, "prune error:", err)
		return 1
	}

	label := "removed"
	if dryRun {
		label = "would remove"
	}
	if len(removed) == 0 {
		fmt.Fprintln(w, "nothing to prune")
		return 0
	}
	for _, r := range removed {
		fmt.Fprintf(w, "%s: %s\n", label, r)
	}
	return 0
}

func init() {
	register(command{"prune", "remove snapshots outside the configured retention policy", func(args []string, stdout, stderr io.Writer) int {
		fs := flag.NewFlagSet("prune", flag.ContinueOnError)
		dryRun := fs.Bool("dry-run", false, "list what would be removed, without removing anything")
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

		// A real prune deletes snapshot directories, so it must hold the
		// data-dir save lock (issue #4); --dry-run is read-only and exempt.
		if !*dryRun {
			release, ok := lockOrFail(store.Dir, stderr)
			if !ok {
				return 1
			}
			defer release()
		}

		return RunPrune(stdout, store.Dir, cfg, *dryRun, time.Now())
	}})
}
