package restore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

// TestApplyRelocationAndContentsReplay builds the relocation plan from
// snapNet() (Task 13) against a live server where default:1 is occupied by a
// differently-named window, so the "rcfiles" window relocates. It asserts
// Apply substitutes the WinPlaceholder ({{WIN}}) in every subsequent action
// of that window with the index tmux's "-P -F" reply reports, and that a
// "contents" action replays scrollback by writing it to <replayDir>/<paneKey>.txt
// and cat'ing it from the pane (RULING R26 — never load-buffer/paste-buffer,
// which would type the saved bytes as keystrokes into a live shell).
func TestApplyRelocationAndContentsReplay(t *testing.T) {
	live := LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}, {1, "tmux-restore"}}}}
	p := BuildPlan(live, snapNet(), Options{Contents: true, ClaudeResumePath: "/home/tim/bin/claude-resume", SeedSession: "default", SeedWindow: "h"})

	f := &tmuxctl.Fake{
		Replies: map[string][]string{
			`new-window -d -P -F "#{window_index}" -t default: -n rcfiles -c /tmp`: {"7"},
		},
		Default: []string{},
	}
	netPaneKey := snapshot.PaneKey("net", 0, 0)
	wantBytes := []byte("hello from net:0.0")
	contentsFn := func(paneKey string) ([]byte, bool) {
		if paneKey == netPaneKey {
			return wantBytes, true
		}
		return nil, false
	}
	replayDir := filepath.Join(t.TempDir(), "replay")

	report, err := Apply(context.Background(), f, p, contentsFn, replayDir)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Nothing fails in this scenario, so the measured counts (RULING R28)
	// should equal the plan's structural ones.
	if report.Created != p.Created || report.Relocated != p.Relocated || report.Skipped != p.Skipped {
		t.Fatalf("measured report counts %+v should match the all-succeeds plan counts %+v", report, p)
	}

	calls := strings.Join(f.Calls, "\n")
	if !strings.Contains(calls, `send-keys -t default:7.0 "'/home/tim/bin/claude-resume' 'abc'" Enter`) {
		t.Errorf("placeholder not substituted in send-keys:\n%s", calls)
	}
	if !strings.Contains(calls, `select-layout -t default:7 "L1"`) {
		t.Errorf("placeholder not substituted in select-layout:\n%s", calls)
	}
	if strings.Contains(calls, WinPlaceholder) {
		t.Errorf("unresolved placeholder leaked into a tmux call:\n%s", calls)
	}
	if strings.Contains(calls, "load-buffer") || strings.Contains(calls, "paste-buffer") {
		t.Errorf("load-buffer/paste-buffer must not be used (RULING R26):\n%s", calls)
	}

	wantFile := filepath.Join(replayDir, netPaneKey+".txt")
	wantCmd := fmt.Sprintf("send-keys -t net:0.0 %s Enter", tmuxQuote(" cat "+shellQuote([]string{wantFile})))
	if !strings.Contains(calls, wantCmd) {
		t.Errorf("expected cat-replay send-keys %q, got:\n%s", wantCmd, calls)
	}

	fi, err := os.Stat(wantFile)
	if err != nil {
		t.Fatalf("replay file %s should exist: %v", wantFile, err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("replay file mode = %v, want 0600", fi.Mode().Perm())
	}
	got, err := os.ReadFile(wantFile)
	if err != nil {
		t.Fatalf("read replay file: %v", err)
	}
	if string(got) != string(wantBytes) {
		t.Errorf("replay file content = %q, want %q", got, wantBytes)
	}
}

// failOnce wraps a *tmuxctl.Fake and forces one specific command to fail
// (a *tmuxctl.CmdError, same shape tmux itself returns), while every other
// command is served normally by the embedded Fake.
type failOnce struct {
	*tmuxctl.Fake
	failCmd string
}

func (f *failOnce) Run(ctx context.Context, cmd string) ([]string, error) {
	if cmd == f.failCmd {
		f.Fake.Calls = append(f.Fake.Calls, cmd)
		return nil, &tmuxctl.CmdError{Cmd: cmd, Lines: []string{"boom"}}
	}
	return f.Fake.Run(ctx, cmd)
}

// TestApplyCreationFailureAbortsOnlyThatBlock covers the error policy: a
// failed window-creating command (here, the relocated "rcfiles" new-window)
// aborts the remaining actions of that window only — no unresolved
// WinPlaceholder is ever sent to tmux — while the unrelated "net" session's
// creation and its actions proceed normally. RULING R28: the failed
// relocation must NOT be counted in Report.Relocated (Apply measures actual
// outcomes, it doesn't copy the plan's structural counts).
func TestApplyCreationFailureAbortsOnlyThatBlock(t *testing.T) {
	live := LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}, {1, "tmux-restore"}}}}
	p := BuildPlan(live, snapNet(), Options{SeedSession: "default", SeedWindow: "h"})

	relocCmd := `new-window -d -P -F "#{window_index}" -t default: -n rcfiles -c /tmp`
	f := &failOnce{Fake: &tmuxctl.Fake{Default: []string{}}, failCmd: relocCmd}

	report, err := Apply(context.Background(), f, p, func(string) ([]byte, bool) { return nil, false }, t.TempDir())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	calls := strings.Join(f.Fake.Calls, "\n")
	if strings.Contains(calls, WinPlaceholder) {
		t.Errorf("unresolved placeholder leaked after a failed relocation:\n%s", calls)
	}
	if !strings.Contains(calls, "new-session -d -s net -n swcfg -c /") {
		t.Errorf("unrelated session creation should still run:\n%s", calls)
	}
	if !strings.Contains(calls, "select-window -t net:0") {
		t.Errorf("unrelated window's own actions should still run:\n%s", calls)
	}

	if report.Relocated != 0 {
		t.Errorf("Relocated = %d, want 0 (the failed relocation must not be counted)", report.Relocated)
	}
	if report.Created != 1 { // only net:0 succeeded
		t.Errorf("Created = %d, want 1", report.Created)
	}
	if report.Skipped != 1 { // default:0 "h" already matches
		t.Errorf("Skipped = %d, want 1", report.Skipped)
	}

	found := false
	for _, n := range report.Notes {
		if strings.Contains(n, relocCmd) && strings.Contains(n, "boom") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a Notes entry describing the failed creation, got %+v", report.Notes)
	}
}

// TestApplyPlainNewWindowFailureAbortsBlock covers FINDING 4: a non-relocated
// "new-window" failure (the saved index was simply free live) must abort
// only that window's own remaining actions — nothing downstream ever
// mentions its target again — while the next session's creation still runs,
// and the failed window must not be counted in Report.Created.
func TestApplyPlainNewWindowFailureAbortsBlock(t *testing.T) {
	live := LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}}}}
	p := BuildPlan(live, snapNet(), Options{SeedSession: "default", SeedWindow: "h"})

	failCmd := "new-window -d -t default:1 -n rcfiles -c /tmp"
	f := &failOnce{Fake: &tmuxctl.Fake{Default: []string{}}, failCmd: failCmd}

	report, err := Apply(context.Background(), f, p, func(string) ([]byte, bool) { return nil, false }, t.TempDir())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	count := 0
	for _, c := range f.Fake.Calls {
		if strings.Contains(c, "default:1") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly the failed new-window call to mention default:1 (no split-window/select-layout/send-keys/select-pane/set-window-option for it), got %d calls:\n%s", count, strings.Join(f.Fake.Calls, "\n"))
	}
	calls := strings.Join(f.Fake.Calls, "\n")
	if !strings.Contains(calls, "new-session -d -s net -n swcfg -c /") {
		t.Errorf("the next session's creation should still run:\n%s", calls)
	}

	if report.Created != 1 { // only net:0 succeeded; default:1 rcfiles must be excluded
		t.Errorf("Created = %d, want 1 (default:1 rcfiles must not be counted)", report.Created)
	}

	found := false
	for _, n := range report.Notes {
		if strings.Contains(n, failCmd) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a Notes entry for the failed new-window, got %+v", report.Notes)
	}
}

// TestApplyNewSessionFailureAbortsWholeSession covers the session-wide half
// of the abort policy (FINDING 2/RULING R28): a failed new-session must
// abort EVERY remaining action of that whole saved session — not just its
// first window — since every later new-window in that session targets a
// session that was never created. A following, unrelated session's creation
// must still run normally.
func TestApplyNewSessionFailureAbortsWholeSession(t *testing.T) {
	snap := &snapshot.Snapshot{Sessions: []snapshot.Session{
		{Name: "net", ActiveWindow: 0, Windows: []snapshot.Window{
			{Index: 0, Name: "swcfg", Layout: "L1", Panes: []snapshot.Pane{{Index: 0, Cwd: "/tmp", Restore: snapshot.Restore{Kind: "shell"}}}},
			{Index: 1, Name: "logs", Layout: "L2", Panes: []snapshot.Pane{{Index: 0, Cwd: "/tmp", Restore: snapshot.Restore{Kind: "shell"}}}},
		}},
		{Name: "other", ActiveWindow: 0, Windows: []snapshot.Window{
			{Index: 0, Name: "w", Layout: "L3", Panes: []snapshot.Pane{{Index: 0, Cwd: "/tmp", Restore: snapshot.Restore{Kind: "shell"}}}},
		}},
	}}
	live := LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}}}} // neither "net" nor "other" exist live
	p := BuildPlan(live, snap, Options{SeedSession: "default", SeedWindow: "h"})

	failCmd := "new-session -d -s net -n swcfg -c /tmp"
	f := &failOnce{Fake: &tmuxctl.Fake{Default: []string{}}, failCmd: failCmd}

	report, err := Apply(context.Background(), f, p, func(string) ([]byte, bool) { return nil, false }, t.TempDir())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	for _, c := range f.Fake.Calls {
		if strings.Contains(c, "net:") {
			t.Errorf("no action should ever target net: after its new-session failed, got call %q", c)
		}
	}
	calls := strings.Join(f.Fake.Calls, "\n")
	if !strings.Contains(calls, "new-session -d -s other -n w -c /tmp") {
		t.Errorf("the following unrelated session's creation should still run:\n%s", calls)
	}

	if report.Created != 1 { // only "other" succeeded; "net" (both windows) must be excluded
		t.Errorf("Created = %d, want 1 (net's windows must not be counted)", report.Created)
	}

	found := false
	for _, n := range report.Notes {
		if strings.Contains(n, failCmd) && strings.Contains(n, "boom") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a Notes entry describing the failed new-session, got %+v", report.Notes)
	}
}

// TestApplyContextCancelledStopsAndReturnsErr covers FINDING 3: Apply checks
// ctx at the top of its loop and returns ctx.Err() immediately once it's
// done, without running any further actions.
func TestApplyContextCancelledStopsAndReturnsErr(t *testing.T) {
	live := LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}}}}
	p := BuildPlan(live, snapNet(), Options{SeedSession: "default", SeedWindow: "h"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f := &tmuxctl.Fake{Default: []string{}}
	_, err := Apply(ctx, f, p, func(string) ([]byte, bool) { return nil, false }, t.TempDir())
	if err == nil {
		t.Fatal("expected a context-cancellation error")
	}
	if len(f.Calls) != 0 {
		t.Errorf("expected no tmux calls once the context is already done, got %v", f.Calls)
	}
}
