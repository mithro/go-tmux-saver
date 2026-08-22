package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
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

// TestRestoreSummaryErrorsSuffixAndExitCode covers FINDING 2's CLI-facing
// half directly and deterministically (no live tmux server needed): when
// any planned creation failed, restoreSummary appends the error-count
// suffix and calls for exit 1; with no errors, no suffix and exit 0.
func TestRestoreSummaryErrorsSuffixAndExitCode(t *testing.T) {
	line, code := restoreSummary(RestoreOutcome{Sessions: 1, Windows: 1, Relocated: 0, Skipped: 0, Errors: 1})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.HasSuffix(line, "(1 errors — see events.log)") {
		t.Errorf("summary = %q, want it to end with the errors suffix", line)
	}

	line0, code0 := restoreSummary(RestoreOutcome{Sessions: 1, Windows: 2, Relocated: 0, Skipped: 1})
	if code0 != 0 {
		t.Errorf("exit code = %d, want 0", code0)
	}
	if strings.Contains(line0, "errors") {
		t.Errorf("summary = %q, should not mention errors when Errors == 0", line0)
	}
}

// failingTransport wraps a *tmuxctl.Fake and forces one specific command to
// fail (a *tmuxctl.CmdError, same shape tmux itself returns), while every
// other command is served normally by the embedded Fake — same pattern as
// internal/restore's own failOnce, reimplemented here since that one is
// unexported in a different package.
type failingTransport struct {
	*tmuxctl.Fake
	failCmd string
}

func (f *failingTransport) Run(ctx context.Context, cmd string) ([]string, error) {
	if cmd == f.failCmd {
		f.Fake.Calls = append(f.Fake.Calls, cmd)
		return nil, &tmuxctl.CmdError{Cmd: cmd, Lines: []string{"boom"}}
	}
	return f.Fake.Run(ctx, cmd)
}

// writeSnapshotDir writes snap as <dir>/layout.json (matching what
// snapshot.Store.Load reads) and returns dir, for tests that want to drive
// RunRestore against a hand-built snapshot without going through a real
// save.
func writeSnapshotDir(t *testing.T, snap *snapshot.Snapshot) string {
	t.Helper()
	snap.Schema = snapshot.SchemaVersion
	dir := t.TempDir()
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "layout.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRunRestoreCountsErrorsFromFailedCreate covers TEST GAP 2 (errors>0
// path) deterministically via RunRestore + a Fake transport that fails one
// new-session, instead of trying to force a real tmux server to fail a
// creation (impractical to arrange reliably): asserts RestoreOutcome.Errors
// == 1 and that the logged "restore" event's Detail carries "errors=1".
func TestRunRestoreCountsErrorsFromFailedCreate(t *testing.T) {
	snap := &snapshot.Snapshot{Sessions: []snapshot.Session{
		{Name: "extra", ActiveWindow: 0, Windows: []snapshot.Window{
			{Index: 0, Name: "w", Layout: "L1", Panes: []snapshot.Pane{
				{Index: 0, Cwd: "/tmp", Restore: snapshot.Restore{Kind: "shell"}},
			}},
		}},
	}}
	snapDir := writeSnapshotDir(t, snap)

	failCmd := `new-session -d -s "extra" -n "w" -c "/tmp"`
	f := &failingTransport{
		Fake: &tmuxctl.Fake{
			Replies: map[string][]string{
				`list-windows -a -F "#{session_name}\t#{window_index}\t#{window_name}\t#{session_grouped}"`: {"default\t0\th\t0"},
				`list-clients -F "#{client_name}"`: {},
			},
			Default: []string{},
		},
		failCmd: failCmd,
	}

	cfg := config.Default()
	cfg.SeedSession = "default"
	cfg.SeedWindow = "h"
	cfg.Contents.Enabled = false

	dataDir := t.TempDir()
	gz, _ := snapshot.LookupCodec("gzip")
	store := &snapshot.Store{Dir: dataDir, Codec: gz}
	if err := store.EnsureDir(); err != nil {
		t.Fatal(err)
	}

	o, err := RunRestore(context.Background(), RestoreDeps{
		T: f, Store: store, Cfg: cfg, SnapshotDir: snapDir,
		PrepareReplayDir: func() (string, error) { return t.TempDir(), nil },
	})
	if err != nil {
		t.Fatalf("RunRestore: %v", err)
	}
	if o.Errors != 1 {
		t.Fatalf("Errors = %d, want 1 (the failed new-session)", o.Errors)
	}
	if o.Windows != 0 {
		t.Errorf("Windows = %d, want 0 (the only planned window failed)", o.Windows)
	}

	line, code := restoreSummary(o)
	if code != 1 {
		t.Errorf("restoreSummary exit code = %d, want 1", code)
	}
	if !strings.HasSuffix(line, "(1 errors — see events.log)") {
		t.Errorf("summary = %q, want the errors suffix", line)
	}

	ev, err := snapshot.TailEvents(dataDir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev[0].Outcome != "restore" {
		t.Fatalf("events %+v, want one restore event", ev)
	}
	if !strings.Contains(ev[0].Detail, "errors=1") {
		t.Errorf("event detail = %q, want it to contain %q", ev[0].Detail, "errors=1")
	}
}
