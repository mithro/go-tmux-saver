package cli

import (
	"bytes"
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

// writeConfigWithSeed is writeTestConfig but with an overridable seed_session,
// for RULING R29's "seed session doesn't even exist" scenario.
func writeConfigWithSeed(t *testing.T, sock, seedSession string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"socket":       sock,
		"seed_session": seedSession,
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

// TestRestoreCLISkipNotSeedOnly covers the ordinary --on-start skip path: a
// live server that already has more than the seed window must not be
// touched — exit 0, "skipped: server not seed-only", no restore attempted.
func TestRestoreCLISkipNotSeedOnly(t *testing.T) {
	sock := tmuxctl.StartTestServer(t)
	exec.Command("tmux", "-L", sock, "new-window", "-d", "-t", "default:1", "-n", "extra").Run()

	cfgFile := writeTestConfig(t, sock)
	dataDir := t.TempDir()
	var out, errb bytes.Buffer
	code := Run([]string{"restore", "--on-start", "--config", cfgFile, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "skipped: server not seed-only") {
		t.Fatalf("stdout = %q, want the skip message", out.String())
	}
}

// TestRestoreCLIMissingSeedSessionOnStartSkips covers RULING R29: on
// --on-start, a live server whose CONFIGURED seed session doesn't exist at
// all (so Dial itself fails attaching, before QueryLive/IsSeedOnly ever run)
// must be treated the same as "not seed-only" — exit 0, skip message — not
// as a genuine error.
func TestRestoreCLIMissingSeedSessionOnStartSkips(t *testing.T) {
	sock := tmuxctl.StartTestServer(t) // creates "default"/"h", not "nonexistent-seed"
	cfgFile := writeConfigWithSeed(t, sock, "nonexistent-seed")
	dataDir := t.TempDir()

	var out, errb bytes.Buffer
	code := Run([]string{"restore", "--on-start", "--config", cfgFile, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "skipped: server not seed-only") {
		t.Fatalf("stdout = %q, want the skip message", out.String())
	}
}

// TestRestoreCLISnapshotDirOverride covers --snapshot DIR: it must restore
// from the EXPLICITLY named snapshot, not silently fall back to the store's
// most recent one.
func TestRestoreCLISnapshotDirOverride(t *testing.T) {
	sock := tmuxctl.StartTestServer(t)
	run := func(args ...string) { exec.Command("tmux", append([]string{"-L", sock}, args...)...).Run() }
	cfgFile := writeTestConfig(t, sock)
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)

	run("new-window", "-d", "-t", "default:1", "-n", "foo", "-c", "/tmp")
	if code := Run([]string{"save", "--config", cfgFile, "--no-display"}, io.Discard, os.Stderr); code != 0 {
		t.Fatal("save (foo) failed")
	}
	snapADir, err := os.Readlink(filepath.Join(dataDir, "go-tmux-saver", "last"))
	if err != nil {
		t.Fatalf("read last symlink: %v", err)
	}
	snapADir = filepath.Join(dataDir, "go-tmux-saver", snapADir)

	// Snapshot dir names have one-second granularity (snap-<ts>, ts truncated
	// to seconds); wait past the boundary so the second save gets a distinct
	// name instead of colliding with the first.
	time.Sleep(1100 * time.Millisecond)

	run("kill-window", "-t", "default:1")
	run("new-window", "-d", "-t", "default:1", "-n", "bar", "-c", "/tmp")
	if code := Run([]string{"save", "--config", cfgFile, "--no-display"}, io.Discard, os.Stderr); code != 0 {
		t.Fatal("save (bar) failed")
	}
	run("kill-window", "-t", "default:1") // back to seed-only

	var out, errb bytes.Buffer
	code := Run([]string{"restore", "--config", cfgFile, "--snapshot", snapADir}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}

	got, _ := exec.Command("tmux", "-L", sock, "list-windows", "-a", "-F", "#{session_name}:#{window_index} #{window_name}").Output()
	if !strings.Contains(string(got), "default:1 foo") {
		t.Fatalf("--snapshot should have restored snapshot A's window (foo), got:\n%s", got)
	}
	if strings.Contains(string(got), "default:1 bar") {
		t.Fatalf("--snapshot must not fall back to the newer snapshot (bar):\n%s", got)
	}
}

// TestRestoreCLINoContentsSkipsReplayFiles covers --no-contents: scrollback
// replay must be skipped entirely (no files written under the replay dir),
// even though the windows/panes themselves are still restored.
func TestRestoreCLINoContentsSkipsReplayFiles(t *testing.T) {
	sock := tmuxctl.StartTestServer(t)
	run := func(args ...string) { exec.Command("tmux", append([]string{"-L", sock}, args...)...).Run() }
	cfgFile := writeTestConfig(t, sock)
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)

	run("new-window", "-d", "-t", "default:1", "-n", "foo", "-c", "/tmp")
	if code := Run([]string{"save", "--config", cfgFile, "--no-display"}, io.Discard, os.Stderr); code != 0 {
		t.Fatal("save failed")
	}
	run("kill-window", "-t", "default:1")

	var out, errb bytes.Buffer
	code := Run([]string{"restore", "--config", cfgFile, "--no-contents"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}

	got, _ := exec.Command("tmux", "-L", sock, "list-windows", "-a", "-F", "#{session_name}:#{window_index} #{window_name}").Output()
	if !strings.Contains(string(got), "default:1 foo") {
		t.Fatalf("window should still be restored with --no-contents, got:\n%s", got)
	}

	files, err := filepath.Glob(filepath.Join(dataDir, "go-tmux-saver", "replay", "*", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("--no-contents should write no replay files, found %v", files)
	}
}

// TestRestoreCLISummaryString pins the exact summary line format.
func TestRestoreCLISummaryString(t *testing.T) {
	sock := tmuxctl.StartTestServer(t)
	run := func(args ...string) { exec.Command("tmux", append([]string{"-L", sock}, args...)...).Run() }
	cfgFile := writeTestConfig(t, sock)
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)

	run("new-window", "-d", "-t", "default:1", "-n", "foo", "-c", "/tmp")
	if code := Run([]string{"save", "--config", cfgFile, "--no-display"}, io.Discard, os.Stderr); code != 0 {
		t.Fatal("save failed")
	}
	run("kill-window", "-t", "default:1")

	var out, errb bytes.Buffer
	code := Run([]string{"restore", "--config", cfgFile}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	want := "restored 1 sessions, 2 windows (0 relocated, 1 skipped)\n"
	if out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
}
