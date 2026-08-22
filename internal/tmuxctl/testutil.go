package tmuxctl

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// StartTestServer starts a throwaway tmux server (session "default", window
// "h") on a unique socket and kills it when the test ends. Skips if tmux is
// not installed.
//
// The server is started with -f /dev/null so it never loads the invoking
// user's ~/.tmux.conf: on a real workstation that config can install hooks,
// a plugin manager, or tmux-continuum, any of which would run against this
// throwaway server (continuum in particular can destroy it mid-test, which
// surfaces as an intermittent "server exited unexpectedly").
func StartTestServer(t testing.TB) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	name := strings.ReplaceAll(t.Name(), "/", "_")
	sock := fmt.Sprintf("gts-test-%d-%s", os.Getpid(), name)
	if out, err := exec.Command("tmux", "-L", sock, "-f", "/dev/null", "new-session", "-d", "-s", "default", "-n", "h", "tail -f /dev/null").CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("tmux", "-L", sock, "kill-server").Run() })
	return sock
}
