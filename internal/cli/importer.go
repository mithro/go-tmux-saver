package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/importer"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

// RunImportResurrect converts the tmux-resurrect save at savePath (and,
// optionally, its sibling pane_contents.tar.gz at contentsTar) into a
// go-tmux-saver snapshot, stages it into store, and — unless promote is
// false — promotes it so it becomes `last`. It prints a one-line summary
// (sessions/windows/panes, and how many panes recovered scrollback) to w
// either way, so a --promote=false dry run still reports what would have
// landed.
func RunImportResurrect(w io.Writer, store *snapshot.Store, savePath, contentsTar, claudeResumePath string, promote bool) int {
	snap, contents, err := importer.FromResurrect(savePath, contentsTar, claudeResumePath)
	if err != nil {
		fmt.Fprintln(w, "import error:", err)
		return 1
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
		fmt.Fprintln(w, "import error:", err)
		return 1
	}
	if promote {
		if _, err := stg.Promote(); err != nil {
			fmt.Fprintln(w, "import error:", err)
			return 1
		}
	} else if err := stg.Discard(); err != nil {
		fmt.Fprintln(w, "import error:", err)
		return 1
	}

	fmt.Fprintf(w, "imported %d sessions, %d windows, %d panes (%d with contents)\n", len(snap.Sessions), windows, panes, withContents)
	return 0
}

const importResurrectUsage = "usage: import-resurrect <savefile> [--contents TAR] [--promote] [--config PATH] [--data-dir DIR]"

func init() {
	register(command{"import-resurrect", "convert a tmux-resurrect save (tmux_resurrect_*.txt [+ pane_contents.tar.gz]) into a go-tmux-saver snapshot", func(args []string, stdout, stderr io.Writer) int {
		// The documented usage is `import-resurrect <savefile> [flags...]`
		// (savefile first) — flag.FlagSet stops parsing at the first
		// non-flag argument, so pull the positional savefile out by hand
		// before handing the rest to fs.Parse.
		savePath, rest, ok := takeFirstPositional(args)
		if !ok {
			fmt.Fprintln(stderr, importResurrectUsage)
			return 2
		}

		fs := flag.NewFlagSet("import-resurrect", flag.ContinueOnError)
		contents := fs.String("contents", "", "path to tmux-resurrect's sibling pane_contents.tar.gz (omit for no pane contents)")
		promote := fs.Bool("promote", true, "promote the imported snapshot to `last` (--promote=false stages then discards, printing counts only)")
		socket := fs.String("socket", "", "override config socket")
		dataDir := fs.String("data-dir", "", "override config data dir")
		cfgPath := fs.String("config", config.Path(), "config file")
		fs.SetOutput(stderr)
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() != 0 {
			fmt.Fprintln(stderr, importResurrectUsage)
			return 2
		}

		cfg, store, msg, code := commonSetup(*cfgPath, *socket, *dataDir)
		if code != 0 {
			fmt.Fprintln(stderr, msg)
			return code
		}

		return RunImportResurrect(stdout, store, savePath, *contents, expandHome(cfg.ClaudeResumePath), *promote)
	}})
}

// takeFirstPositional pulls the first non-flag ("-"-prefixed) argument out
// of args, returning it plus the remaining args in original order (for
// flag.FlagSet.Parse). ok is false if args has no non-flag argument.
func takeFirstPositional(args []string) (positional string, rest []string, ok bool) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			rest = append(append([]string(nil), args[:i]...), args[i+1:]...)
			return a, rest, true
		}
	}
	return "", args, false
}
