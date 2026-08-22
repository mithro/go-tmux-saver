package restore

import (
	"strings"
	"testing"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

func snapNet() *snapshot.Snapshot {
	return &snapshot.Snapshot{Sessions: []snapshot.Session{
		{Name: "default", ActiveWindow: 1, Windows: []snapshot.Window{
			{Index: 0, Name: "h", Layout: "L0", Panes: []snapshot.Pane{{Index: 0, Cwd: "/home/tim", Restore: snapshot.Restore{Kind: "shell"}}}},
			{Index: 1, Name: "rcfiles", Layout: "L1", AutomaticRename: false, Panes: []snapshot.Pane{{Index: 0, Cwd: "/home/tim/rcfiles", Active: true, Restore: snapshot.Restore{Kind: "claude", ClaudeSession: "abc"}}}},
		}},
		{Name: "net", ActiveWindow: 0, Windows: []snapshot.Window{
			{Index: 0, Name: "swcfg", Layout: "L2", Panes: []snapshot.Pane{
				{Index: 0, Cwd: "/home/tim/net", Active: true, Restore: snapshot.Restore{Kind: "argv", Argv: []string{"ssh", "sw it's"}}},
				{Index: 1, Cwd: "/nonexistent/dir", Restore: snapshot.Restore{Kind: "shell"}}}}}},
	}}
}

func TestIsSeedOnly(t *testing.T) {
	seed := LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}}}}
	if !IsSeedOnly(seed, "default", "h") {
		t.Fatal("seed should be seed-only")
	}
	if IsSeedOnly(LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}, {1, "x"}}}}, "default", "h") {
		t.Fatal("extra window is not seed-only")
	}
	if IsSeedOnly(LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}}, "net": {}}}, "default", "h") {
		t.Fatal("extra session is not seed-only")
	}
}

func TestPlanOnSeedServer(t *testing.T) {
	t.Setenv("HOME", "/home/tim")
	live := LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}}}}
	p := BuildPlan(live, snapNet(), Options{ClaudeResumePath: "/home/tim/bin/claude-resume", Contents: true, SeedSession: "default", SeedWindow: "h"})
	cmds := strings.Join(flatten(p), "\n")
	for _, want := range []string{
		"new-window -d -t default:1 -n rcfiles -c /home/tim/rcfiles",
		"new-session -d -s net -n swcfg -c /home/tim/net",
		"split-window -d -t net:0 -c /home/tim", // missing cwd → $HOME fallback (HOME=/home/tim in test via t.Setenv)
		`select-layout -t net:0 "L2"`,
		`send-keys -t net:0.0 'ssh' 'sw it'\''s' Enter`,
		`send-keys -t default:1.0 '/home/tim/bin/claude-resume' 'abc' Enter`,
		"select-window -t net:0",
		"select-window -t default:1",
	} {
		if !strings.Contains(cmds, want) {
			t.Errorf("plan missing %q\n%s", want, cmds)
		}
	}
	if strings.Contains(cmds, "rename-window -t default:0") || strings.Contains(cmds, "new-window -d -t default:0") {
		t.Error("seed window must not be touched")
	}
	if p.Created != 2 || p.Skipped != 1 || p.Relocated != 0 { // created rcfiles + net:0; default:0 h skipped (same name)
		t.Errorf("counts %+v", p)
	}
}

func TestPlanRelocatesOnConflict(t *testing.T) {
	live := LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}, {1, "tmux-restore"}}}}
	p := BuildPlan(live, snapNet(), Options{SeedSession: "default", SeedWindow: "h"})
	cmds := strings.Join(flatten(p), "\n")
	if strings.Contains(cmds, "rename-window -t default:1") || strings.Contains(cmds, "new-window -d -t default:1 ") {
		t.Fatalf("must never touch occupied window default:1\n%s", cmds)
	}
	if !strings.Contains(cmds, "new-window -d -t default: -n rcfiles") || p.Relocated != 1 {
		t.Fatalf("expected relocation\n%s %+v", cmds, p)
	}
}

func flatten(p Plan) []string {
	var out []string
	for _, a := range p.Actions {
		if a.Kind == "tmux" {
			out = append(out, a.Args[0])
		}
	}
	return out
}
