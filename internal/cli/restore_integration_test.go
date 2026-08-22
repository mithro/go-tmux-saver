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

// TestSaveRestoreRoundTrip proves the whole pipeline against a real tmux
// server: save a layout beyond the seed window, kill it live, then
// `restore --on-start` must recreate it (relocated windows included) and
// relaunch the allowlisted "tail" process that was running in net:0.0.
func TestSaveRestoreRoundTrip(t *testing.T) {
	sock := tmuxctl.StartTestServer(t)
	run := func(args ...string) { exec.Command("tmux", append([]string{"-L", sock}, args...)...).Run() }
	run("new-window", "-d", "-t", "default:1", "-n", "editor", "-c", "/tmp")
	run("split-window", "-d", "-t", "default:1", "-c", "/")
	run("new-session", "-d", "-s", "net", "-n", "swcfg", "-c", "/tmp")
	run("send-keys", "-t", "net:0", "tail -f /dev/null", "Enter")
	time.Sleep(300 * time.Millisecond)

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
	out, _ := exec.Command("tmux", "-L", sock, "list-windows", "-a", "-F", "#{session_name}:#{window_index} #{window_name} #{window_panes}").Output()
	got := string(out)
	for _, want := range []string{"default:0 h 1", "default:1 editor 2", "net:0 swcfg 1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("after restore missing %q:\n%s", want, got)
		}
	}
	time.Sleep(500 * time.Millisecond)
	cmd, _ := exec.Command("tmux", "-L", sock, "display-message", "-p", "-t", "net:0.0", "#{pane_current_command}").Output()
	if strings.TrimSpace(string(cmd)) != "tail" {
		t.Fatalf("tail should have been relaunched in net:0.0, got %q", cmd)
	}
}
