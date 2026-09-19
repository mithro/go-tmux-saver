// Package tmuxtest holds test-only helpers that keep go-tmux-saver's tests
// from seeing or touching a real (production) tmux server running on the
// same host. It is imported only from _test.go files, so it is never linked
// into the release binary.
package tmuxtest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
)

var socketSeq atomic.Uint64

// SocketName returns a unique tmux socket name for a test, short enough that
// <TMUX_TMPDIR>/tmux-$UID/<name> stays within the ~108-byte sun_path limit
// even when TMUX_TMPDIR is a real per-user directory rather than /tmp. It
// keeps a truncated, sanitised tag (usually t.Name()) for debuggability, and
// a process-unique sequence number so two tags that collide after truncation
// still get distinct sockets. Callers pass it to `tmux -L <name>`.
func SocketName(tag string) string {
	tag = strings.Map(func(r rune) rune {
		if r == '/' || r == ' ' || r == ';' || r == ':' {
			return '_'
		}
		return r
	}, tag)
	const maxTag = 24
	if len(tag) > maxTag {
		tag = tag[:maxTag]
	}
	return fmt.Sprintf("gts-%d-%d-%s", os.Getpid(), socketSeq.Add(1), tag)
}

// Isolate relocates the tmux socket directory for the whole test process
// into a private, throwaway directory (unless TMUX_TMPDIR is already set),
// runs the tests via run, tears the directory down, and returns the exit
// code for os.Exit.
//
// tmux places every server's socket at $TMUX_TMPDIR/tmux-$UID/<name>, with
// TMUX_TMPDIR defaulting to /tmp. A workstation or router's production
// server therefore lives at /tmp/tmux-$UID/main — the same directory the
// tests would drop their own -L sockets into. By pointing TMUX_TMPDIR at a
// throwaway directory before any test starts a server, every test socket
// lands under that directory instead, so a test can neither see, reuse, nor
// (via a stray `-L main`) clobber the real server. TMUX is also unset so a
// test never inherits an attach to the caller's own session.
//
// An already-set TMUX_TMPDIR is respected and left untouched: the sandboxed
// test runner (scripts/sandboxed-test.py) sets it to a scratch dir it owns,
// and the two must compose rather than fight.
//
// Wire it into a package's TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(tmuxtest.Isolate(m.Run)) }
func Isolate(run func() int) int {
	os.Unsetenv("TMUX")

	if os.Getenv("TMUX_TMPDIR") != "" {
		// An outer sandbox already isolated us; don't override or remove it.
		return run()
	}

	// Prefer the per-user runtime tmpfs (avoids /tmp and is the natural home
	// for sockets); fall back to the OS temp dir when it is unavailable. The
	// prefix is kept short: this dir becomes TMUX_TMPDIR, and a socket at
	// <dir>/tmux-$UID/<name> must fit the ~108-byte sun_path limit.
	dir, err := os.MkdirTemp(os.Getenv("XDG_RUNTIME_DIR"), "gts-tt-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tmuxtest: cannot create a private TMUX_TMPDIR:", err)
		return 1
	}
	os.Setenv("TMUX_TMPDIR", dir)

	code := run()

	// Best-effort teardown: kill any server that outlived its test (each
	// test kills its own via t.Cleanup, so this only catches leaks), then
	// remove the directory.
	killServers(dir)
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "tmuxtest: removing private TMUX_TMPDIR:", err)
	}
	return code
}

// killServers kills any tmux server whose socket lives under tmuxTmpdir.
// Errors are surfaced except the benign "no server" case, so a genuine
// teardown problem (e.g. a wedged server) stays visible.
func killServers(tmuxTmpdir string) {
	socks, _ := filepath.Glob(filepath.Join(tmuxTmpdir, "tmux-*", "*"))
	for _, sock := range socks {
		out, err := exec.Command("tmux", "-S", sock, "kill-server").CombinedOutput()
		if err != nil && !isNoServer(out) {
			fmt.Fprintf(os.Stderr, "tmuxtest: kill-server %s: %v: %s\n", sock, err, out)
		}
	}
}

// isNoServer reports whether kill-server's output is the benign "there was
// nothing to kill" case (a dead socket left by a test that already cleaned
// up its own server), as opposed to a real failure worth surfacing.
func isNoServer(out []byte) bool {
	s := string(out)
	for _, benign := range []string{"no server running", "no such file or directory", "error connecting"} {
		if strings.Contains(s, benign) {
			return true
		}
	}
	return len(strings.TrimSpace(s)) == 0
}
