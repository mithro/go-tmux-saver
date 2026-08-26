package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/mithro/go-tmux-saver/internal/resume"
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

func init() {
	register(command{"claude-resume", "confirm, then resume a specific Claude session (the placeholder a restore types into Claude panes)", func(args []string, stdout, stderr io.Writer) int {
		sid := ""
		if len(args) > 0 {
			sid = args[0]
		}
		home, _ := os.UserHomeDir()
		projects := filepath.Join(home, ".claude", "projects")

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
