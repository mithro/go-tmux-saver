package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/importer"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

// RunImportResurrect converts the tmux-resurrect save at savePath (and,
// optionally, its sibling pane_contents.tar.gz at contentsTar) into a
// go-tmux-saver snapshot, stages it into store, and — unless promote is
// false — promotes it so it becomes `last`. Every warning FromResurrect
// returns (one per skipped/malformed save-file line or unmatched tar entry,
// see internal/importer's package doc) is printed to stderr; the one-line
// summary on stdout (sessions/windows/panes, how many panes recovered
// scrollback) gains a " (S lines skipped)" suffix when S>0. This is a
// best-effort import — a nonzero S alone doesn't fail the command (exit 0)
// unless strict is set, in which case it exits 1 (the summary and
// staging/promotion still happen either way, so --strict is purely a
// stronger signal for scripting, not a dry run).
func RunImportResurrect(stdout, stderr io.Writer, store *snapshot.Store, savePath, contentsTar, claudeResumePath string, promote, strict bool) int {
	snap, contents, warnings, err := importer.FromResurrect(savePath, contentsTar, claudeResumePath)
	if err != nil {
		fmt.Fprintln(stderr, "import error:", err)
		return 1
	}
	for _, msg := range warnings {
		fmt.Fprintln(stderr, "warning:", msg)
	}

	panes, windows := snap.CountPanes()
	withContents := 0
	for _, se := range snap.Sessions {
		for _, win := range se.Windows {
			for _, p := range win.Panes {
				if _, ok := contents[snapshot.PaneKey(se.Name, win.Index, p.Index)]; ok {
					withContents++
				}
			}
		}
	}

	stg, err := store.Stage(snap, contents)
	if err != nil {
		fmt.Fprintln(stderr, "import error:", err)
		return 1
	}
	if promote {
		if _, err := stg.Promote(); err != nil {
			fmt.Fprintln(stderr, "import error:", err)
			return 1
		}
		// A promoted import IS the store's current good snapshot, so record
		// it like one: without this, `status` right after rollout step 2
		// reported STALE with an empty event log even though a perfectly
		// good snapshot had just been installed.
		if err := snapshot.AppendEvent(store.Dir, snapshot.Event{
			Time: time.Now(), Outcome: "kept",
			Panes: panes, Windows: windows, Sessions: len(snap.Sessions),
			Detail: "import-resurrect",
		}); err != nil {
			fmt.Fprintln(stderr, "warning: events.log:", err)
		}
		if err := snapshot.TouchFresh(store.Dir); err != nil {
			fmt.Fprintln(stderr, "warning: fresh marker:", err)
		}
	} else if err := stg.Discard(); err != nil {
		fmt.Fprintln(stderr, "import error:", err)
		return 1
	}

	summary := fmt.Sprintf("imported %d sessions, %d windows, %d panes (%d with contents)", len(snap.Sessions), windows, panes, withContents)
	if len(warnings) > 0 {
		summary += fmt.Sprintf(" (%d lines skipped)", len(warnings))
	}
	fmt.Fprintln(stdout, summary)

	if strict && len(warnings) > 0 {
		return 1
	}
	return 0
}

const importResurrectUsage = "usage: import-resurrect [flags] <savefile>  |  import-resurrect <savefile> [flags]"

func init() {
	register(command{"import-resurrect", "convert a tmux-resurrect save (tmux_resurrect_*.txt [+ pane_contents.tar.gz]) into a go-tmux-saver snapshot", func(args []string, stdout, stderr io.Writer) int {
		fs := flag.NewFlagSet("import-resurrect", flag.ContinueOnError)
		contents := fs.String("contents", "", "path to tmux-resurrect's sibling pane_contents.tar.gz (omit for no pane contents)")
		promote := fs.Bool("promote", true, "promote the imported snapshot to `last` (--promote=false stages then discards, printing counts only)")
		strict := fs.Bool("strict", false, "exit 1 if any save-file lines or tar entries were skipped (summary/warnings are still printed either way)")
		socket := fs.String("socket", "", "override config socket")
		dataDir := fs.String("data-dir", "", "override config data dir")
		cfgPath := fs.String("config", config.Path(), "config file")
		fs.SetOutput(stderr)

		// RULING R36: if args[0] doesn't look like a flag, it's the savefile
		// and the rest are flags (`import-resurrect <savefile> [flags]`);
		// otherwise the whole of args is parsed as flags and the savefile
		// must be the sole remaining positional arg
		// (`import-resurrect [flags] <savefile>`). This supports both
		// orderings without the ambiguity of scanning for "the first
		// non-flag token anywhere" (which could mistake a flag's value for
		// the savefile).
		var savePath string
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			savePath = args[0]
			if err := fs.Parse(args[1:]); err != nil {
				return 2
			}
			if fs.NArg() != 0 {
				fmt.Fprintln(stderr, importResurrectUsage)
				return 2
			}
		} else {
			if err := fs.Parse(args); err != nil {
				return 2
			}
			if fs.NArg() != 1 {
				fmt.Fprintln(stderr, importResurrectUsage)
				return 2
			}
			savePath = fs.Arg(0)
		}

		cfg, store, msg, code := commonSetup(*cfgPath, *socket, *dataDir)
		if code != 0 {
			fmt.Fprintln(stderr, msg)
			return code
		}

		return RunImportResurrect(stdout, stderr, store, savePath, *contents, expandHome(cfg.ClaudeResumePath), *promote, *strict)
	}})
}
