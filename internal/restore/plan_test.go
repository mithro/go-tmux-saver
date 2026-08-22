package restore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

func snapNet() *snapshot.Snapshot {
	return &snapshot.Snapshot{Sessions: []snapshot.Session{
		{Name: "default", ActiveWindow: 1, Windows: []snapshot.Window{
			{Index: 0, Name: "h", Layout: "L0", Panes: []snapshot.Pane{{Index: 0, Cwd: "/tmp", Restore: snapshot.Restore{Kind: "shell"}}}},
			{Index: 1, Name: "rcfiles", Layout: "L1", AutomaticRename: false, Panes: []snapshot.Pane{{Index: 0, Cwd: "/tmp", Active: true, Restore: snapshot.Restore{Kind: "claude", ClaudeSession: "abc"}}}},
		}},
		{Name: "net", ActiveWindow: 0, Windows: []snapshot.Window{
			{Index: 0, Name: "swcfg", Layout: "L2", Panes: []snapshot.Pane{
				{Index: 0, Cwd: "/", Active: true, ContentFile: "net_0_0.txt.gz", Restore: snapshot.Restore{Kind: "argv", Argv: []string{"ssh", "sw it's"}}},
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
		`new-window -d -t "default:1" -n "rcfiles" -c "/tmp"`,
		`new-session -d -s "net" -n "swcfg" -c "/"`,
		`split-window -d -t "net:0" -c "/home/tim"`, // missing cwd → $HOME fallback (HOME=/home/tim in test via t.Setenv)
		`select-layout -t "net:0" "L2"`,
		`send-keys -t "net:0.0" "'ssh' 'sw it'\\''s'" Enter`,
		`send-keys -t "default:1.0" "'/home/tim/bin/claude-resume' 'abc'" Enter`,
		`select-window -t "net:0"`,
		`select-window -t "default:1"`,
	} {
		if !strings.Contains(cmds, want) {
			t.Errorf("plan missing %q\n%s", want, cmds)
		}
	}
	if strings.Contains(cmds, `rename-window -t "default:0"`) || strings.Contains(cmds, `new-window -d -t "default:0"`) {
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
	if strings.Contains(cmds, `rename-window -t "default:1"`) || strings.Contains(cmds, `new-window -d -t "default:1" `) {
		t.Fatalf("must never touch occupied window default:1\n%s", cmds)
	}
	if !strings.Contains(cmds, `new-window -d -P -F "#{window_index}" -t "default:" -n "rcfiles" -c "/tmp"`) || p.Relocated != 1 {
		t.Fatalf("expected relocation\n%s %+v", cmds, p)
	}
}

// TestPlanSkipsRelocationWhenSameNameExistsElsewhere covers RULING R27:
// relocation is not idempotent unless a window already relocated (or
// otherwise present under its saved name at some other index) is recognised
// and left alone on the next run, rather than relocated again — the saved
// index stays occupied by the foreign window forever, so without this check
// every re-run would pile up another copy.
func TestPlanSkipsRelocationWhenSameNameExistsElsewhere(t *testing.T) {
	live := LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}, {1, "tmux-restore"}, {7, "rcfiles"}}}}
	p := BuildPlan(live, snapNet(), Options{SeedSession: "default", SeedWindow: "h"})
	cmds := strings.Join(flatten(p), "\n")
	if strings.Contains(cmds, "new-window -d -P -F") {
		t.Fatalf("must not relocate when a window with the same name already exists live\n%s", cmds)
	}
	found := false
	for _, a := range p.Actions {
		if a.Kind == "note" && a.Note == "present at index 7" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a %q skip note, actions: %+v", "present at index 7", p.Actions)
	}
	// default:0 "h" (already-present skip) + default:1 "rcfiles" (present
	// elsewhere skip) = 2 skipped; net:0 "swcfg" is still created fresh.
	if p.Skipped != 2 || p.Created != 1 || p.Relocated != 0 {
		t.Fatalf("counts %+v", p)
	}
}

// TestPlanSelectWindowNotDeferredAcrossRelocations covers RULING R23: with
// two saved windows in one existing session both relocated (their saved
// indices are occupied live by differently-named windows), the
// select-window for the session's ActiveWindow must be emitted right after
// that window's own actions — before the second relocation's new-window
// action — not deferred to the end of the session's loop. WinPlaceholder is
// a single fixed token ("{{WIN}}"), so a deferred select-window would be
// ambiguous about which relocation it refers to once a second relocation
// has happened.
func TestPlanSelectWindowNotDeferredAcrossRelocations(t *testing.T) {
	snap := &snapshot.Snapshot{Sessions: []snapshot.Session{
		{Name: "default", ActiveWindow: 1, Windows: []snapshot.Window{
			{Index: 1, Name: "win1", Layout: "L1", Panes: []snapshot.Pane{{Index: 0, Cwd: "/tmp", Restore: snapshot.Restore{Kind: "shell"}}}},
			{Index: 2, Name: "win2", Layout: "L2", Panes: []snapshot.Pane{{Index: 0, Cwd: "/tmp", Restore: snapshot.Restore{Kind: "shell"}}}},
		}},
	}}
	live := LiveState{Sessions: map[string][]LiveWindow{"default": {{1, "other1"}, {2, "other2"}}}}
	p := BuildPlan(live, snap, Options{})

	selectIdx, secondRelocIdx, relocCount, selectCount := -1, -1, 0, 0
	for i, a := range p.Actions {
		if a.Kind != "tmux" {
			continue
		}
		cmd := a.Args[0]
		if strings.HasPrefix(cmd, `new-window -d -P -F "#{window_index}"`) {
			relocCount++
			if relocCount == 2 {
				secondRelocIdx = i
			}
		}
		if cmd == `select-window -t "default:{{WIN}}"` {
			selectCount++
			if selectIdx == -1 {
				selectIdx = i
			}
		}
	}
	if relocCount != 2 {
		t.Fatalf("expected 2 relocations, got %d\n%v", relocCount, flatten(p))
	}
	if selectCount != 1 {
		t.Fatalf("expected exactly one select-window, got %d\n%v", selectCount, flatten(p))
	}
	if selectIdx == -1 || secondRelocIdx == -1 || selectIdx >= secondRelocIdx {
		t.Fatalf("select-window (idx %d) must appear before the second relocation's new-window (idx %d)\n%v", selectIdx, secondRelocIdx, flatten(p))
	}
}

// TestPlanEmptyArgvNoSendKeys covers RULING R24: an "argv" pane with no
// saved argv is treated like "shell" — no send-keys is emitted for it.
func TestPlanEmptyArgvNoSendKeys(t *testing.T) {
	snap := &snapshot.Snapshot{Sessions: []snapshot.Session{
		{Name: "default", ActiveWindow: 0, Windows: []snapshot.Window{
			{Index: 1, Name: "empty", Layout: "L1", Panes: []snapshot.Pane{{Index: 0, Cwd: "/tmp", Restore: snapshot.Restore{Kind: "argv", Argv: nil}}}},
		}},
	}}
	live := LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}}}}
	p := BuildPlan(live, snap, Options{})
	for _, cmd := range flatten(p) {
		if strings.HasPrefix(cmd, "send-keys") {
			t.Errorf("unexpected send-keys for empty argv: %q", cmd)
		}
	}
}

// TestPlanArgvDollarSignEscapedForTmux covers RULING R30: tmux's own
// double-quote syntax expands "$NAME" (verified directly against this
// tmux via control mode), so an unescaped "$HOME" spliced into a
// send-keys command line would be resolved by TMUX ITSELF before the pane
// ever sees it — not what a saved "echo $HOME" argv means. The emitted
// command must contain the tmux-escaped "\$HOME", and must not contain any
// UNESCAPED occurrence of "$HOME" (i.e. one without a preceding backslash).
func TestPlanArgvDollarSignEscapedForTmux(t *testing.T) {
	snap := &snapshot.Snapshot{Sessions: []snapshot.Session{
		{Name: "extra", ActiveWindow: 0, Windows: []snapshot.Window{
			{Index: 0, Name: "w", Layout: "L0", Panes: []snapshot.Pane{
				{Index: 0, Cwd: "/tmp", Restore: snapshot.Restore{Kind: "argv", Argv: []string{"echo", "$HOME"}}},
			}},
		}},
	}}
	live := LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}}}} // "extra" doesn't exist live: created fresh
	p := BuildPlan(live, snap, Options{SeedSession: "default", SeedWindow: "h"})
	cmds := strings.Join(flatten(p), "\n")

	if !strings.Contains(cmds, `\$HOME`) {
		t.Fatalf("expected tmux-escaped \\$HOME in the plan, got:\n%s", cmds)
	}
	if strings.Contains(strings.ReplaceAll(cmds, `\$HOME`, ""), "$HOME") {
		t.Fatalf("found an UNESCAPED $HOME (tmux would expand it itself) in:\n%s", cmds)
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

// TestPlanQuotesDataDerivedArguments covers C1: session names, window
// names, cwds and every composed "-t sess:idx[.pane]" target are attacker-
// influenced data (a tmux session can be named anything), so each must be
// spliced into the tmux command line as ONE tmuxQuote'd argument. Before
// this fix a session named `evil;kill-window -t default:0` executed
// kill-window against the live server (probe-confirmed), and a cwd
// containing a space split into two arguments.
func TestPlanQuotesDataDerivedArguments(t *testing.T) {
	base := t.TempDir()
	cwdSpace := filepath.Join(base, "dir with space")
	cwdSemi := filepath.Join(base, "semi;colon dir")
	for _, d := range []string{cwdSpace, cwdSemi} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	evil := "evil;kill-window -t default:0"
	snap := &snapshot.Snapshot{Sessions: []snapshot.Session{
		{Name: evil, ActiveWindow: 0, Windows: []snapshot.Window{
			{Index: 0, Name: "a;b c", Layout: "L 0", Panes: []snapshot.Pane{
				{Index: 0, Cwd: cwdSpace, Active: true, Restore: snapshot.Restore{Kind: "argv", Argv: []string{"echo", "hi there"}}},
				{Index: 1, Cwd: cwdSemi},
			}},
		}},
		{Name: "sess two", ActiveWindow: 9, Windows: []snapshot.Window{
			{Index: 3, Name: "reloc me", Layout: "L 3", Panes: []snapshot.Pane{{Index: 0, Cwd: cwdSpace}}},
			{Index: 9, Name: "free idx", Layout: "L 9", AutomaticRename: true, Panes: []snapshot.Pane{{Index: 0, Cwd: cwdSemi}}},
		}},
	}}
	live := LiveState{Sessions: map[string][]LiveWindow{"sess two": {{3, "someone else"}}}}

	p := BuildPlan(live, snap, Options{Contents: false})
	cmds := flatten(p)
	joined := strings.Join(cmds, "\n")

	for _, want := range []string{
		`new-session -d -s "evil;kill-window -t default:0" -n "a;b c" -c "` + cwdSpace + `"`,
		`split-window -d -t "evil;kill-window -t default:0:0" -c "` + cwdSemi + `"`,
		`select-layout -t "evil;kill-window -t default:0:0" "L 0"`,
		`send-keys -t "evil;kill-window -t default:0:0.0" "'echo' 'hi there'" Enter`,
		`select-pane -t "evil;kill-window -t default:0:0.0"`,
		`set-window-option -t "evil;kill-window -t default:0:0" automatic-rename off`,
		`select-window -t "evil;kill-window -t default:0:0"`,
		`new-window -d -P -F "#{window_index}" -t "sess two:" -n "reloc me" -c "` + cwdSpace + `"`,
		`new-window -d -t "sess two:9" -n "free idx" -c "` + cwdSemi + `"`,
		`select-layout -t "sess two:{{WIN}}" "L 3"`,
		`select-window -t "sess two:9"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("plan missing %q\n%s", want, joined)
		}
	}

	// Nothing data-derived may appear outside a quoted argument: every
	// emitted command line's arguments after the verb must be either a
	// tmux-literal token (no space/semicolon) or a "…" quoted string.
	for _, cmd := range cmds {
		if strings.Contains(unquotedParts(cmd), ";") {
			t.Errorf("unquoted %q in command line: %q", ";", cmd)
		}
	}
}

// unquotedParts returns cmd with every "…"-quoted span (honouring
// backslash escapes) removed, i.e. only the parts tmux itself parses as
// command structure. Used to prove no data-derived text escapes its quotes.
func unquotedParts(cmd string) string {
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if inQuote && c == '\\' {
			i++
			continue
		}
		if c == '"' {
			inQuote = !inQuote
			continue
		}
		if !inQuote {
			b.WriteByte(c)
		}
	}
	return b.String()
}
