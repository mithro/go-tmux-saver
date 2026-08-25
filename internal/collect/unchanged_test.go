package collect

import (
	"testing"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

func base() *snapshot.Snapshot {
	return &snapshot.Snapshot{Sessions: []snapshot.Session{{Name: "s", Windows: []snapshot.Window{{Index: 0, Name: "w", Layout: "L",
		Panes: []snapshot.Pane{{Index: 0, Cwd: "/a", Title: "tim@ten64: ~", HistoryLines: 5, Restore: snapshot.Restore{Kind: "shell"}}}}}}}}
}

func TestUnchanged(t *testing.T) {
	a, b := base(), base()
	b.Stats.Panes = 99
	b.Sessions[0].Windows[0].Panes[0].HistoryLines = 500
	b.Sessions[0].Windows[0].Panes[0].ContentSHA256 = "zzz"
	b.Sessions[0].Windows[0].Panes[0].Title = "tim@ten64: ~/elsewhere" // shell title churn
	if !Unchanged(a, b) {
		t.Fatal("metadata/shell-title changes must be ignored")
	}
	c := base()
	c.Sessions[0].Windows[0].Panes[0].Cwd = "/b"
	if Unchanged(a, c) {
		t.Fatal("cwd change is a change")
	}
	d, e := base(), base()
	d.Sessions[0].Windows[0].Panes[0].Title = "✳ fixing dns"
	e.Sessions[0].Windows[0].Panes[0].Title = "✳ fixing dhcp"
	if Unchanged(d, e) {
		t.Fatal("claude ✳ titles are state")
	}
	f := base()
	f.Sessions[0].Windows = append(f.Sessions[0].Windows, snapshot.Window{Index: 1, Name: "x"})
	if Unchanged(a, f) {
		t.Fatal("extra window is a change")
	}
}

// TestUnchangedAsymmetricClaudeTitle covers the issue-#8 gap from Task 10:
// when only ONE side has a ✳ Claude summary title (Claude started or
// exited between saves, the other side showing a plain shell title), that
// transition is state and must count as a change — in both directions.
func TestUnchangedAsymmetricClaudeTitle(t *testing.T) {
	a, b := base(), base()
	a.Sessions[0].Windows[0].Panes[0].Title = "tim@ten64: ~"
	b.Sessions[0].Windows[0].Panes[0].Title = "✳ fixing dns"
	if Unchanged(a, b) {
		t.Fatal("shell → ✳ transition is a change")
	}
	if Unchanged(b, a) {
		t.Fatal("✳ → shell transition is a change")
	}
}
