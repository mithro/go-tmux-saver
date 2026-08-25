package cli

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

// writeTestConfig writes a JSON config overlay pointing at sock, with the
// seed session/window matching tmuxctl.StartTestServer ("default"/"h") and
// the built-in default allowlist (which includes "tail"), and returns its
// path.
func writeTestConfig(t *testing.T, sock string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"socket":       sock,
		"seed_session": "default",
		"seed_window":  "h",
	})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// waitFor polls check every 50ms until it reports ok, or fails the test once
// timeout elapses, reporting the last observed state. Used in place of fixed
// sleeps so the test doesn't flake under load (RULING R25) while still
// failing fast once tmux genuinely never reaches the expected state.
func waitFor(t *testing.T, timeout time.Duration, check func() (ok bool, state string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, state := check()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s; last observed state:\n%s", timeout, state)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSaveRestoreRoundTrip proves the whole pipeline against a real tmux
// server: save a layout beyond the seed window, kill it live, then
// `restore --on-start` must recreate it (relocated windows included) and
// relaunch the allowlisted "tail" process that was running in net:0.0.
func TestSaveRestoreRoundTrip(t *testing.T) {
	sock := tmuxctl.StartTestServer(t)
	run := func(args ...string) { exec.Command("tmux", append([]string{"-L", sock}, args...)...).Run() }
	listWindows := func() string {
		out, _ := exec.Command("tmux", "-L", sock, "list-windows", "-a", "-F", "#{session_name}:#{window_index} #{window_name} #{window_panes}").Output()
		return string(out)
	}
	paneCmd := func(target string) string {
		out, _ := exec.Command("tmux", "-L", sock, "display-message", "-p", "-t", target, "#{pane_current_command}").Output()
		return strings.TrimSpace(string(out))
	}
	wantWindows := []string{"default:0 h 1", "default:1 editor 2", "net:0 swcfg 1"}
	haveAllWindows := func(got string) bool {
		for _, want := range wantWindows {
			if !strings.Contains(got, want) {
				return false
			}
		}
		return true
	}

	run("new-window", "-d", "-t", "default:1", "-n", "editor", "-c", "/tmp")
	run("split-window", "-d", "-t", "default:1", "-c", "/")
	run("new-session", "-d", "-s", "net", "-n", "swcfg", "-c", "/tmp")
	run("send-keys", "-t", "net:0", "tail -f /dev/null", "Enter")

	// Wait for the pre-save layout to fully settle: all three windows present
	// with their expected pane counts, and "tail" actually exec'd in net:0.0
	// (not still "bash" mid-fork), before snapshotting it.
	waitFor(t, 5*time.Second, func() (bool, string) {
		got := listWindows()
		if !haveAllWindows(got) {
			return false, got
		}
		if cmd := paneCmd("net:0.0"); cmd != "tail" {
			return false, got + "net:0.0 pane_current_command=" + cmd + "\n"
		}
		return true, ""
	})

	dataDir := t.TempDir()
	cfgFile := writeTestConfig(t, sock)
	t.Setenv("XDG_DATA_HOME", dataDir)
	if code := Run([]string{"save", "--config", cfgFile, "--no-display"}, io.Discard, os.Stderr); code != 0 {
		t.Fatal("save failed")
	}
	run("kill-session", "-t", "net")
	run("kill-window", "-t", "default:1")
	if code := Run([]string{"restore", "--on-start", "--config", cfgFile}, io.Discard, os.Stderr); code != 0 {
		t.Fatal("restore failed")
	}

	waitFor(t, 5*time.Second, func() (bool, string) {
		got := listWindows()
		return haveAllWindows(got), got
	})
	got := listWindows()
	for _, want := range wantWindows {
		if !strings.Contains(got, want) {
			t.Fatalf("after restore missing %q:\n%s", want, got)
		}
	}

	waitFor(t, 5*time.Second, func() (bool, string) {
		cmd := paneCmd("net:0.0")
		return cmd == "tail", cmd
	})
	if cmd := paneCmd("net:0.0"); cmd != "tail" {
		t.Fatalf("tail should have been relaunched in net:0.0, got %q", cmd)
	}
}

// TestSaveRestoreRoundTripHostileNames covers the issue-#8 gap: the C1
// quoting fix (tmuxctl.Quote on every data-derived argument) is exercised
// against a REAL tmux server with names containing double quotes,
// backslashes, spaces, semicolons and a dot — the probe-confirmed
// injection/mangling characters. (':' is deliberately absent: sessions
// with ':' in the name are unaddressable and refused — issue #5.)
//
// The assertions compare against the names tmux actually STORED, not the
// argv we passed: probe-verified (3.5a and next-3.8) that tmux's CLI argv
// path doubles backslashes at creation time (new-session -s 'a\b' stores
// a\\b — has-session confirms), while -F output and control-mode command
// parsing are verbatim. The property under test is round-trip fidelity of
// the stored server state, whatever its spelling.
func TestSaveRestoreRoundTripHostileNames(t *testing.T) {
	sock := tmuxctl.StartTestServer(t)
	sess := `q"uo\te.s;s`
	win := `w"in\d ; x`
	run := func(args ...string) {
		if out, err := exec.Command("tmux", append([]string{"-L", sock}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
	}
	listWindows := func() string {
		out, _ := exec.Command("tmux", "-L", sock, "list-windows", "-a", "-F", "#{session_name}:#{window_index} #{window_name} #{window_panes}").Output()
		return string(out)
	}

	run("new-session", "-d", "-s", sess, "-n", win, "-c", "/tmp")
	// Find the hostile session's id and its STORED window line (see the doc
	// comment: the stored spelling can differ from the argv spelling).
	out, err := exec.Command("tmux", "-L", sock, "list-sessions", "-F", "#{session_id}\t#{session_name}").Output()
	if err != nil {
		t.Fatalf("list-sessions: %v", err)
	}
	sid := ""
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if id, name, ok := strings.Cut(line, "\t"); ok && name != "default" {
			sid = id
		}
	}
	if sid == "" {
		t.Fatalf("hostile session not found in list-sessions output: %q", out)
	}
	run("split-window", "-d", "-t", sid+":0")

	want := ""
	waitFor(t, 5*time.Second, func() (bool, string) {
		for _, line := range strings.Split(strings.TrimSpace(listWindows()), "\n") {
			if !strings.HasPrefix(line, "default:") && strings.HasSuffix(line, " 2") {
				want = line
				return true, ""
			}
		}
		return false, listWindows()
	})
	if !strings.ContainsAny(want, `"\;.`) {
		t.Fatalf("stored window line lost its hostile characters: %q", want)
	}

	dataDir := t.TempDir()
	cfgFile := writeTestConfig(t, sock)
	t.Setenv("XDG_DATA_HOME", dataDir)
	if code := Run([]string{"save", "--config", cfgFile, "--no-display"}, io.Discard, os.Stderr); code != 0 {
		t.Fatal("save failed")
	}
	run("kill-session", "-t", sid)
	if code := Run([]string{"restore", "--on-start", "--config", cfgFile}, io.Discard, os.Stderr); code != 0 {
		t.Fatal("restore failed")
	}
	waitFor(t, 5*time.Second, func() (bool, string) {
		got := listWindows()
		return strings.Contains(got, want), got
	})
	// The seed must have survived untouched — a quoting break here would
	// have let the hostile name execute as extra tmux commands (C1).
	if got := listWindows(); !strings.Contains(got, "default:0 h") {
		t.Fatalf("seed window damaged:\n%s", got)
	}
}
