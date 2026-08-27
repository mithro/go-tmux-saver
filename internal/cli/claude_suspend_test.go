package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mithro/go-tmux-saver/internal/procs"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

const suspendSID = "11111111-2222-3333-4444-555555555555" // registry fixture 101.json

// suspendFixture builds SuspendDeps around the procs testdata (pane pid 100
// → claude pid 101) plus an "after" /proc dir where claude has exited.
// scanFlip decides which table each Scan call returns.
func suspendFixture(t *testing.T, f *tmuxctl.Fake, claudeExits bool) (SuspendDeps, *strings.Builder) {
	t.Helper()
	after := t.TempDir()
	for pid, comm, ppid := 100, "bash", 1; pid != 0; pid = 0 {
		d := filepath.Join(after, "100")
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		stat := "100 (" + comm + ") S " + "1" + " 100 100 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 5000 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n"
		_ = ppid
		if err := os.WriteFile(filepath.Join(d, "stat"), []byte(stat), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "cmdline"), []byte("bash\x00"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	var out strings.Builder
	d := SuspendDeps{
		T:   f,
		Reg: procs.ClaudeRegistry{Dir: "../procs/testdata/sessions"},
		Scan: func() (*procs.Table, error) {
			calls++
			if claudeExits && calls > 1 {
				return procs.Scan(after)
			}
			return procs.Scan("../procs/testdata/proc")
		},
		Allowlist: procs.DefaultAllowlist,
		Exe:       "/usr/bin/go-tmux-saver",
		SavedDir:  filepath.Join(t.TempDir(), "suspend"),
		Out:       &out,
		Sleep:     func(time.Duration) {},
		ExitTimeout: 2 * time.Second,
	}
	return d, &out
}

func suspendFake() *tmuxctl.Fake {
	return &tmuxctl.Fake{
		Replies: map[string][]string{
			"list-panes -t \"=default:1\" -F \"#{pane_id}\t#{pane_pid}\"":      {"%5\t100"},
			"list-windows -t \"=default\" -F \"#{window_index}\t#{window_name}\"": {"0\th", "1\trcfiles"},
			"capture-pane -epJ -S - -t \"%5\"":                               {"old console line 1", "old console line 2"},
		},
		Default: []string{},
	}
}

// TestRunSuspendByIndex: the full happy path — capture, /exit + Enter,
// exit confirmed on the second Scan, placeholder typed with the capture
// file, all in that order.
func TestRunSuspendByIndex(t *testing.T) {
	f := suspendFake()
	d, out := suspendFixture(t, f, true)
	done, failed, err := RunSuspend(context.Background(), d, "default", "1", false)
	if err != nil || done != 1 || failed != 0 {
		t.Fatalf("done=%d failed=%d err=%v\n%s", done, failed, err, out.String())
	}
	joined := strings.Join(f.Calls, "\n")
	iExit := strings.Index(joined, `send-keys -t "%5" "/exit"`)
	iEnter := strings.Index(joined, `send-keys -t "%5" Enter`)
	iPlaceholder := strings.Index(joined, "claude-resume")
	if iExit < 0 || iEnter < 0 || iPlaceholder < 0 || !(iExit < iEnter && iEnter < iPlaceholder) {
		t.Fatalf("call order wrong (exit=%d enter=%d placeholder=%d):\n%s", iExit, iEnter, iPlaceholder, joined)
	}
	if !strings.Contains(joined, "'"+suspendSID+"' '--saved-output'") {
		t.Fatalf("placeholder must carry sid then --saved-output:\n%s", joined)
	}
	files, _ := filepath.Glob(filepath.Join(d.SavedDir, "*.txt"))
	if len(files) != 1 {
		t.Fatalf("capture files = %v, want 1", files)
	}
	if data, _ := os.ReadFile(files[0]); !strings.Contains(string(data), "old console line 2") {
		t.Fatalf("capture content = %q", data)
	}
	if !strings.Contains(out.String(), "suspended default:1 %5") {
		t.Fatalf("output = %q", out.String())
	}
}

// TestRunSuspendByName resolves the window name via list-windows.
func TestRunSuspendByName(t *testing.T) {
	f := suspendFake()
	d, _ := suspendFixture(t, f, true)
	done, failed, err := RunSuspend(context.Background(), d, "default", "rcfiles", false)
	if err != nil || done != 1 || failed != 0 {
		t.Fatalf("done=%d failed=%d err=%v", done, failed, err)
	}
}

// TestRunSuspendConfirmFailure: Claude never exits → bounded failure, NO
// placeholder typed into the still-running session.
func TestRunSuspendConfirmFailure(t *testing.T) {
	f := suspendFake()
	d, out := suspendFixture(t, f, false)
	d.ExitTimeout = 1 * time.Millisecond
	done, failed, err := RunSuspend(context.Background(), d, "default", "1", false)
	if err != nil || done != 0 || failed != 1 {
		t.Fatalf("done=%d failed=%d err=%v", done, failed, err)
	}
	if strings.Contains(strings.Join(f.Calls, "\n"), "claude-resume") {
		t.Fatal("placeholder must not be typed while claude still runs")
	}
	if !strings.Contains(out.String(), "still running") {
		t.Fatalf("output = %q, want the still-running error", out.String())
	}
}

// TestRunSuspendAll sweeps every canonical window; non-claude panes are
// silently skipped.
func TestRunSuspendAll(t *testing.T) {
	f := suspendFake()
	f.Replies[`list-windows -a -F "#{session_name}\t#{window_index}\t#{window_name}\t#{session_grouped}\t#{session_group}"`] = []string{
		"default\t0\th\t0\t",
		"default\t1\trcfiles\t0\t",
	}
	f.Replies["list-panes -t \"=default:0\" -F \"#{pane_id}\t#{pane_pid}\""] = []string{"%1\t300"} // ssh pane: skipped
	f.Replies[`list-clients -F "#{client_name}"`] = []string{}
	d, _ := suspendFixture(t, f, true)
	done, failed, err := RunSuspend(context.Background(), d, "", "", true)
	if err != nil || done != 1 || failed != 0 {
		t.Fatalf("done=%d failed=%d err=%v", done, failed, err)
	}
}
