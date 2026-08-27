package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/resume"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

// execveFn/lookPathFn are syscall.Exec and exec.LookPath, swappable in
// tests (CI has no `claude` on PATH, and a test must never really exec):
// on success execveFn never returns — the placeholder becomes the claude
// process, so the pane's process tree, and the next save's /proc
// resolution, see a real `claude`.
var (
	execveFn   = syscall.Exec
	lookPathFn = exec.LookPath
)

// isTTY reports whether f is a terminal (the placeholder styles its banner
// and waits for a keypress only when a human is on the other end). Decided
// by the TCGETS ioctl, like isatty(3) — a mode check on ModeCharDevice
// would misclassify /dev/null.
func isTTY(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

// readLineInterruptible reads one line from r, treating SIGINT (Ctrl-C at
// the pane prompt) like the rcfiles script's KeyboardInterrupt: an error
// return, which Decide turns into "skip — leave a shell in this pane".
func readLineInterruptible(r io.Reader) (string, error) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	type lineResult struct {
		s   string
		err error
	}
	ch := make(chan lineResult, 1)
	go func() {
		br := bufio.NewReader(r)
		s, err := br.ReadString('\n')
		ch <- lineResult{s, err}
	}()
	select {
	case <-sig:
		return "", errors.New("interrupted")
	case res := <-ch:
		return res.s, res.err
	}
}

// savedFromStore finds the last snapshot's pane whose restore is this
// Claude session and returns its saved scrollback (issue #15) — the
// best-effort source when claude-suspend didn't hand over a capture file.
func savedFromStore(store *snapshot.Store, sid string) []byte {
	snap, dir, err := store.Last()
	if err != nil {
		return nil
	}
	for _, se := range snap.Sessions {
		for _, w := range se.Windows {
			for _, p := range w.Panes {
				if p.Restore.Kind == "claude" && p.Restore.ClaudeSession == sid {
					data, err := store.ReadContent(dir, p)
					if err != nil {
						return nil
					}
					return data
				}
			}
		}
	}
	return nil
}

func init() {
	register(command{"claude-resume", "confirm, then resume a specific Claude session (the placeholder a restore types into Claude panes)", func(args []string, stdout, stderr io.Writer) int {
		fs := flag.NewFlagSet("claude-resume", flag.ContinueOnError)
		savedFile := fs.String("saved-output", "", "print this file's content above the banner (claude-suspend's pane capture)")
		noSaved := fs.Bool("no-saved", false, "print no saved console output above the banner")
		savedLines := fs.Int("saved-lines", 100, "how many trailing lines of store-looked-up scrollback to print (0 = all; --saved-output files always print whole)")
		socket := fs.String("socket", "", "override config socket (unused; accepted for uniformity)")
		dataDir := fs.String("data-dir", "", "override config data dir (for the saved-output store lookup)")
		cfgPath := fs.String("config", config.Path(), "config file")
		fs.SetOutput(stderr)
		// Like import-resurrect (RULING R36): the session id may come first
		// (`claude-resume <sid> --saved-output f`) — generated command lines
		// use that order so /proc re-detection keeps matching
		// `claude-resume <uuid>` — or flags may come first.
		sid := ""
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			sid = args[0]
			if err := fs.Parse(args[1:]); err != nil {
				return 2
			}
		} else {
			if err := fs.Parse(args); err != nil {
				return 2
			}
			if fs.NArg() > 0 {
				sid = fs.Arg(0)
			}
		}

		home, _ := os.UserHomeDir()
		projects := filepath.Join(home, ".claude", "projects")

		// Issue #15: reproduce the pane's last console state above the
		// banner. An explicit capture file wins; else best-effort store
		// lookup by session id. Failures here are silent — the banner and
		// resume must never be blocked by missing context.
		if !*noSaved {
			if *savedFile != "" {
				if data, err := os.ReadFile(*savedFile); err == nil {
					stdout.Write(data)
				}
			} else if sid != "" {
				if _, store, _, code := commonSetup(*cfgPath, *socket, *dataDir); code == 0 {
					if data := savedFromStore(store, sid); data != nil {
						stdout.Write(resume.TailLines(data, *savedLines))
					}
				}
			}
		}

		d := resume.Decide(stdout, home, projects, sid,
			isTTY(os.Stdout), isTTY(os.Stdin),
			func() (string, error) { return readLineInterruptible(os.Stdin) })
		if d.Skip {
			return 0
		}
		if d.Chdir != "" {
			if err := os.Chdir(d.Chdir); err != nil {
				fmt.Fprintln(stderr, "claude-resume: chdir:", err)
			}
		}
		path, err := lookPathFn(d.Argv[0])
		if err != nil {
			fmt.Fprintln(stderr, "claude-resume: `claude` not found on PATH")
			return 127
		}
		if err := execveFn(path, d.Argv, os.Environ()); err != nil {
			fmt.Fprintln(stderr, "claude-resume: exec:", err)
			return 127
		}
		return 0 // unreachable: exec replaced the process
	}})
}
