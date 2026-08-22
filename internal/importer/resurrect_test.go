package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

const claudeResumePath = "~/bin/claude-resume"

// TestFromResurrectFixture drives FromResurrect against a fixture trimmed
// from a real tmux-resurrect save (see testdata/tmux_resurrect_sample.txt),
// covering: session/window/pane counts, a claude restore recovering its
// uuid, an argv (ssh) restore, layout strings, active markers, the
// client-session mapping, and pane contents landing under the right
// snapshot.PaneKey from the sibling tarball.
func TestFromResurrectFixture(t *testing.T) {
	snap, contents, warnings, err := FromResurrect("testdata/tmux_resurrect_sample.txt", "testdata/pane_contents.tar.gz", claudeResumePath)
	if err != nil {
		t.Fatalf("FromResurrect: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none (fixture is well-formed)", warnings)
	}

	if snap.Schema != snapshot.SchemaVersion {
		t.Fatalf("schema = %d, want %d", snap.Schema, snapshot.SchemaVersion)
	}
	if snap.TmuxVersion != "imported-resurrect" {
		t.Fatalf("tmux version = %q, want %q", snap.TmuxVersion, "imported-resurrect")
	}
	if snap.Client.Session != "work" {
		t.Fatalf("client session = %q, want %q", snap.Client.Session, "work")
	}

	panes, windows := snap.CountPanes()
	if len(snap.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(snap.Sessions))
	}
	if windows != 3 {
		t.Fatalf("windows = %d, want 3", windows)
	}
	if panes != 4 {
		t.Fatalf("panes = %d, want 4", panes)
	}

	// Sessions are sorted by name: "ops" before "work".
	if snap.Sessions[0].Name != "ops" || snap.Sessions[1].Name != "work" {
		t.Fatalf("session order = %v", []string{snap.Sessions[0].Name, snap.Sessions[1].Name})
	}

	ops := snap.Sessions[0]
	if len(ops.Windows) != 1 {
		t.Fatalf("ops windows = %d, want 1", len(ops.Windows))
	}
	opsWin := ops.Windows[0]
	if opsWin.Layout != "abcd,80x24,0,0,0" {
		t.Fatalf("ops window layout = %q", opsWin.Layout)
	}
	if !opsWin.Active {
		t.Fatalf("ops window should be active")
	}
	if ops.ActiveWindow != opsWin.Index {
		t.Fatalf("ops.ActiveWindow = %d, want %d", ops.ActiveWindow, opsWin.Index)
	}
	if opsWin.AutomaticRename {
		t.Fatalf("ops window automatic-rename should be false (resurrect value %q)", "off")
	}
	if len(opsWin.Panes) != 1 {
		t.Fatalf("ops window panes = %d, want 1", len(opsWin.Panes))
	}
	sshPane := opsWin.Panes[0]
	if sshPane.Restore.Kind != "argv" {
		t.Fatalf("ssh pane restore kind = %q, want argv", sshPane.Restore.Kind)
	}
	if len(sshPane.Restore.Argv) != 2 || sshPane.Restore.Argv[0] != "ssh" || sshPane.Restore.Argv[1] != "host.example" {
		t.Fatalf("ssh pane argv = %v, want [ssh host.example]", sshPane.Restore.Argv)
	}
	if !sshPane.Active {
		t.Fatalf("ssh pane should be marked active")
	}
	if sshPane.Cwd != "/home/tim/proj" {
		t.Fatalf("ssh pane cwd = %q", sshPane.Cwd)
	}

	work := snap.Sessions[1]
	if len(work.Windows) != 2 {
		t.Fatalf("work windows = %d, want 2", len(work.Windows))
	}
	editor := work.Windows[0]
	if editor.Name != "editor" {
		t.Fatalf("work window 0 name = %q, want editor", editor.Name)
	}
	if editor.Layout != "efab,80x24,0,0,1" {
		t.Fatalf("editor layout = %q", editor.Layout)
	}
	if !editor.AutomaticRename {
		t.Fatalf("editor window automatic-rename should be true (resurrect value %q)", "on")
	}
	if len(editor.Panes) != 2 {
		t.Fatalf("editor panes = %d, want 2", len(editor.Panes))
	}
	shellPane, claudePane := editor.Panes[0], editor.Panes[1]
	if shellPane.Restore.Kind != "shell" {
		t.Fatalf("editor pane 0 restore kind = %q, want shell", shellPane.Restore.Kind)
	}
	if shellPane.Active {
		t.Fatalf("editor pane 0 should not be active")
	}
	if claudePane.Restore.Kind != "claude" {
		t.Fatalf("editor pane 1 restore kind = %q, want claude", claudePane.Restore.Kind)
	}
	if claudePane.Restore.ClaudeSession != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("editor pane 1 claude session = %q", claudePane.Restore.ClaudeSession)
	}
	if !claudePane.Active {
		t.Fatalf("editor pane 1 (claude) should be active")
	}

	logs := work.Windows[1]
	if logs.Name != "logs" || logs.Active {
		t.Fatalf("work window 1 = %+v, want name=logs inactive", logs)
	}
	if logs.AutomaticRename {
		t.Fatalf("logs window automatic-rename should be false (resurrect value %q = unset)", ":")
	}
	if len(logs.Panes) != 1 || logs.Panes[0].Restore.Kind != "shell" {
		t.Fatalf("logs window panes = %+v", logs.Panes)
	}

	// Pane contents: the tarball covers 3 of the 4 panes (ops:0.0, work:0.0,
	// work:0.1) — work:1.0 (the "logs" pane) has no tarball entry and must
	// simply be absent from the map, not an error.
	wantKeys := map[string]string{
		snapshot.PaneKey("ops", opsWin.Index, sshPane.Index):     "Welcome to host.example",
		snapshot.PaneKey("work", editor.Index, shellPane.Index):  "bash",
		snapshot.PaneKey("work", editor.Index, claudePane.Index): "Resume Claude session aaaaaaaa",
	}
	if len(contents) != 3 {
		t.Fatalf("contents = %d entries, want 3: %v", len(contents), keysOf(contents))
	}
	for key, wantSubstr := range wantKeys {
		data, ok := contents[key]
		if !ok {
			t.Fatalf("contents missing key %q (have %v)", key, keysOf(contents))
		}
		if !strings.Contains(string(data), wantSubstr) {
			t.Fatalf("contents[%q] = %q, want substring %q", key, data, wantSubstr)
		}
	}
	logsKey := snapshot.PaneKey("work", logs.Index, logs.Panes[0].Index)
	if _, ok := contents[logsKey]; ok {
		t.Fatalf("contents should not have an entry for %q (no tarball entry)", logsKey)
	}
}

// TestFromResurrectNoContents covers contentsTar == "" (no sibling
// pane_contents.tar.gz to import from) — it must succeed with an empty
// contents map, not error.
func TestFromResurrectNoContents(t *testing.T) {
	snap, contents, warnings, err := FromResurrect("testdata/tmux_resurrect_sample.txt", "", claudeResumePath)
	if err != nil {
		t.Fatalf("FromResurrect: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if len(contents) != 0 {
		t.Fatalf("contents = %v, want empty", contents)
	}
	if len(snap.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(snap.Sessions))
	}
}

// TestFromResurrectMalformedLinesWarn covers RULING R37: a save file with
// one well-formed pane line, one 10-field (truncated) pane line, and one
// wholly unrecognized line must import the good pane while producing
// exactly one warning per bad line, each mentioning its 1-based line
// number.
func TestFromResurrectMalformedLinesWarn(t *testing.T) {
	content := strings.Join([]string{
		"pane\tops\t0\t1\t:*\t0\tt\t:/x\t1\tssh\t:ssh h",
		"this is not a resurrect line at all",
		"pane\tops\t1\t0\t:\t0\tt2\t:/y\t1\tbash",
		"window\tops\t0\t:w\t1\t:*\tlay,1\toff",
		"state\tops\tops",
	}, "\n") + "\n"
	path := filepath.Join(t.TempDir(), "malformed.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	snap, _, warnings, err := FromResurrect(path, "", claudeResumePath)
	if err != nil {
		t.Fatalf("FromResurrect: %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want exactly 2", warnings)
	}
	if !strings.Contains(warnings[0], "line 2:") {
		t.Fatalf("warnings[0] = %q, want it to mention line 2", warnings[0])
	}
	if !strings.Contains(warnings[1], "line 3:") {
		t.Fatalf("warnings[1] = %q, want it to mention line 3", warnings[1])
	}

	// The one well-formed pane (line 1, in window ops:0) still imported.
	if len(snap.Sessions) != 1 || snap.Sessions[0].Name != "ops" {
		t.Fatalf("sessions = %+v, want just [ops]", snap.Sessions)
	}
	panes, _ := snap.CountPanes()
	if panes != 1 {
		t.Fatalf("panes = %d, want 1 (only the well-formed pane line)", panes)
	}
}

// TestFromResurrectTabsInTitleAndWindowName covers RULING R38: pane_title
// and window_name can contain embedded tab characters (tmux allows
// arbitrary bytes there) — the right-to-left field parsing must still
// recover the correct dir/command for the pane and the correct name for
// the window, rather than misaligning every field after the tab.
func TestFromResurrectTabsInTitleAndWindowName(t *testing.T) {
	content := strings.Join([]string{
		"pane\twork\t0\t1\t:*\t0\thas\ttab\t:/home/tim\t1\tbash\t:",
		"window\twork\t0\t:win\tname\t1\t:*\tlay,1\ton",
		"state\twork\twork",
	}, "\n") + "\n"
	path := filepath.Join(t.TempDir(), "tabs.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	snap, _, warnings, err := FromResurrect(path, "", claudeResumePath)
	if err != nil {
		t.Fatalf("FromResurrect: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if len(snap.Sessions) != 1 {
		t.Fatalf("sessions = %+v, want 1", snap.Sessions)
	}
	work := snap.Sessions[0]
	if len(work.Windows) != 1 {
		t.Fatalf("windows = %+v, want 1", work.Windows)
	}
	win := work.Windows[0]
	if win.Name != "win\tname" {
		t.Fatalf("window name = %q, want %q", win.Name, "win\tname")
	}
	if len(win.Panes) != 1 {
		t.Fatalf("panes = %+v, want 1", win.Panes)
	}
	p := win.Panes[0]
	if p.Title != "has\ttab" {
		t.Fatalf("pane title = %q, want %q", p.Title, "has\ttab")
	}
	if p.Cwd != "/home/tim" {
		t.Fatalf("pane cwd = %q, want /home/tim", p.Cwd)
	}
	if p.Restore.Kind != "shell" {
		t.Fatalf("pane restore kind = %q, want shell", p.Restore.Kind)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
