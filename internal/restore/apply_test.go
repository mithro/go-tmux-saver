package restore

import (
	"context"
	"os"
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
// "contents" action replays via load-buffer + paste-buffer, removing its
// temp file afterwards.
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
	contentsFn := func(paneKey string) ([]byte, bool) {
		if paneKey == netPaneKey {
			return []byte("hello from net:0.0"), true
		}
		return nil, false
	}

	report, err := Apply(context.Background(), f, p, contentsFn)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if report.Created != p.Created || report.Relocated != p.Relocated || report.Skipped != p.Skipped {
		t.Fatalf("report counts %+v do not mirror plan counts %+v", report, p)
	}

	calls := strings.Join(f.Calls, "\n")
	if !strings.Contains(calls, "send-keys -t default:7.0 '/home/tim/bin/claude-resume' 'abc' Enter") {
		t.Errorf("placeholder not substituted in send-keys:\n%s", calls)
	}
	if !strings.Contains(calls, `select-layout -t default:7 "L1"`) {
		t.Errorf("placeholder not substituted in select-layout:\n%s", calls)
	}
	if strings.Contains(calls, WinPlaceholder) {
		t.Errorf("unresolved placeholder leaked into a tmux call:\n%s", calls)
	}

	var tmpPath string
	loadIdx, pasteIdx := -1, -1
	for i, c := range f.Calls {
		if strings.HasPrefix(c, "load-buffer -b gts ") {
			loadIdx = i
			tmpPath = strings.TrimPrefix(c, "load-buffer -b gts ")
		}
		if c == "paste-buffer -d -b gts -t net:0.0" {
			pasteIdx = i
		}
	}
	if loadIdx == -1 || pasteIdx == -1 {
		t.Fatalf("expected load-buffer and paste-buffer -t net:0.0 calls, got:\n%s", calls)
	}
	if loadIdx >= pasteIdx {
		t.Fatalf("load-buffer (idx %d) must precede paste-buffer (idx %d):\n%s", loadIdx, pasteIdx, calls)
	}
	if tmpPath == "" {
		t.Fatal("no temp file path captured from load-buffer call")
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("temp file %s should have been removed after Apply, stat err = %v", tmpPath, err)
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
// creation and its actions proceed normally.
func TestApplyCreationFailureAbortsOnlyThatBlock(t *testing.T) {
	live := LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}, {1, "tmux-restore"}}}}
	p := BuildPlan(live, snapNet(), Options{SeedSession: "default", SeedWindow: "h"})

	relocCmd := `new-window -d -P -F "#{window_index}" -t default: -n rcfiles -c /tmp`
	f := &failOnce{Fake: &tmuxctl.Fake{Default: []string{}}, failCmd: relocCmd}

	report, err := Apply(context.Background(), f, p, func(string) ([]byte, bool) { return nil, false })
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
