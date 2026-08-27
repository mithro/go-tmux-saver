package resume

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// writeTranscript materialises a synthetic session transcript under a
// projects dir whose folder name is the munge of launchCwd (as Claude Code
// lays it out), and returns the projects dir.
func writeTranscript(t *testing.T, launchCwd string, lines ...string) string {
	t.Helper()
	projects := t.TempDir()
	dir := filepath.Join(projects, Munge(launchCwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return projects
}

func TestMunge(t *testing.T) {
	if got := Munge("/home/tim/github/x.y"); got != "-home-tim-github-x-y" {
		t.Fatalf("Munge = %q", got)
	}
}

func TestReadMetaAndLabel(t *testing.T) {
	launch := t.TempDir()
	projects := writeTranscript(t, launch,
		`{"type":"user","cwd":"`+launch+`","gitBranch":"main","timestamp":"2026-08-26T01:00:00Z","message":{"content":[{"type":"text","text":"fix the dns  outage"}]}}`,
		`{"type":"summary","summary":"DNS outage debugging"}`,
		`{"type":"assistant","cwd":"`+launch+`/wt","timestamp":"2026-08-26T02:00:00Z"}`,
	)
	tr := FindTranscript(projects, sid)
	if tr == "" {
		t.Fatal("transcript not found")
	}
	m, ok := ReadMeta(tr)
	if !ok {
		t.Fatal("ReadMeta failed")
	}
	if m.LaunchCwd != launch || m.WorkCwd != launch+"/wt" || m.Branch != "main" || m.LastTS != "2026-08-26T02:00:00Z" {
		t.Fatalf("meta = %+v", m)
	}
	// summary beats first_user; a title would beat both
	if m.Label() != "DNS outage debugging" {
		t.Fatalf("label = %q", m.Label())
	}
	m.Title = "custom title"
	if m.Label() != "custom title" {
		t.Fatalf("label = %q", m.Label())
	}
	m2 := Meta{FirstUser: "  fix   the\tthing  "}
	if m2.Label() != "fix the thing" {
		t.Fatalf("whitespace collapse: %q", m2.Label())
	}
	if (Meta{}).Label() != "(no summary found)" {
		t.Fatal("empty label fallback")
	}
}

func TestChdirTarget(t *testing.T) {
	launch := t.TempDir()
	projects := writeTranscript(t, launch, `{"type":"user","cwd":"`+launch+`","timestamp":"2026-08-26T01:00:00Z"}`)
	tr := FindTranscript(projects, sid)
	m, _ := ReadMeta(tr)
	if got := ChdirTarget(m, tr); got != launch {
		t.Fatalf("ChdirTarget = %q, want %q", got, launch)
	}
	// launch dir gone → no chdir
	m2 := m
	m2.LaunchCwd = filepath.Join(launch, "gone")
	if got := ChdirTarget(m2, tr); got != "" {
		t.Fatalf("missing dir should give no target, got %q", got)
	}
	// munge mismatch (transcript from a different project) → no chdir
	other := t.TempDir()
	m3 := m
	m3.LaunchCwd = other
	if got := ChdirTarget(m3, tr); got != "" {
		t.Fatalf("munge mismatch should give no target, got %q", got)
	}
}

func TestDecideResumeOnEnter(t *testing.T) {
	launch := t.TempDir()
	projects := writeTranscript(t, launch,
		`{"type":"user","cwd":"`+launch+`","timestamp":"2026-08-26T01:00:00Z","message":{"content":"do the thing"}}`)
	var out bytes.Buffer
	d := Decide(&out, "/home/tim", projects, sid, false, true, func() (string, error) { return "", nil })
	if d.Skip || len(d.Argv) != 3 || d.Argv[0] != "claude" || d.Argv[2] != sid {
		t.Fatalf("decision = %+v", d)
	}
	if d.Chdir != launch {
		t.Fatalf("Chdir = %q, want %q", d.Chdir, launch)
	}
	if !strings.Contains(out.String(), "Resume Claude session") || !strings.Contains(out.String(), "Enter = resume · Ctrl-C = shell") {
		t.Fatalf("banner = %q", out.String())
	}
	if !strings.Contains(out.String(), "↳ resuming") {
		t.Fatalf("announce missing: %q", out.String())
	}
	if !strings.Contains(out.String(), "do the thing") {
		t.Fatalf("label missing: %q", out.String())
	}
}

func TestDecideSkipOnInterrupt(t *testing.T) {
	var out bytes.Buffer
	d := Decide(&out, "", t.TempDir(), sid, false, true, func() (string, error) { return "", errors.New("interrupt") })
	if !d.Skip {
		t.Fatalf("decision = %+v, want skip", d)
	}
	if !strings.Contains(out.String(), "skipped — shell ready") {
		t.Fatalf("skip announce missing: %q", out.String())
	}
}

func TestDecideNonTTYResumesImmediately(t *testing.T) {
	var out bytes.Buffer
	called := false
	d := Decide(&out, "", t.TempDir(), sid, false, false, func() (string, error) { called = true; return "", nil })
	if d.Skip || called {
		t.Fatalf("non-tty must resume without waiting (skip=%v called=%v)", d.Skip, called)
	}
	if !strings.Contains(out.String(), "transcript not found — will still try to resume") {
		t.Fatalf("missing-transcript note absent: %q", out.String())
	}
}

func TestDecideJunkSidFallsBackToPicker(t *testing.T) {
	var out bytes.Buffer
	d := Decide(&out, "", t.TempDir(), "not-a-uuid", false, false, nil)
	if d.Skip || len(d.Argv) != 1 || d.Argv[0] != "claude" {
		t.Fatalf("decision = %+v, want plain claude picker", d)
	}
	if !strings.Contains(out.String(), "no session id — picker") {
		t.Fatalf("picker banner missing: %q", out.String())
	}
}

// TestChdirTargetWorktreeShapes covers issue #17: the resume-time chdir
// must land in the session's LAUNCH directory across the worktree layouts
// in real use. `claude --resume` is project-scoped by the CURRENT dir's
// munged name, so resuming from the wrong place loses the session.
func TestChdirTargetWorktreeShapes(t *testing.T) {
	sid2 := "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	mk := func(t *testing.T, launch, work string) (Meta, string) {
		t.Helper()
		projects := t.TempDir()
		dir := filepath.Join(projects, Munge(launch))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		lines := `{"type":"user","cwd":"` + launch + `","timestamp":"2026-08-27T01:00:00Z"}` + "\n"
		if work != "" {
			lines += `{"type":"assistant","cwd":"` + work + `","timestamp":"2026-08-27T02:00:00Z"}` + "\n"
		}
		tr := filepath.Join(dir, sid2+".jsonl")
		if err := os.WriteFile(tr, []byte(lines), 0o600); err != nil {
			t.Fatal(err)
		}
		m, ok := ReadMeta(tr)
		if !ok {
			t.Fatal("ReadMeta")
		}
		return m, tr
	}

	root := t.TempDir()
	main := filepath.Join(root, "repo")
	for _, d := range []string{main, filepath.Join(main, ".worktrees", "feat")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Launched in the main repo, work moved into a worktree INSIDE it:
	// resume from the main repo (the launch cwd), never the worktree.
	m, tr := mk(t, main, filepath.Join(main, ".worktrees", "feat"))
	if got := ChdirTarget(m, tr); got != main {
		t.Errorf("inner worktree: ChdirTarget = %q, want launch dir %q", got, main)
	}

	// 2. Launched IN the inner worktree: the worktree IS the project.
	wt := filepath.Join(main, ".worktrees", "feat")
	m, tr = mk(t, wt, "")
	if got := ChdirTarget(m, tr); got != wt {
		t.Errorf("launched-in-worktree: ChdirTarget = %q, want %q", got, wt)
	}

	// 3. Global worktree directory (superpowers-style).
	global := filepath.Join(root, ".config", "superpowers", "worktrees", "repo-feat")
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Fatal(err)
	}
	m, tr = mk(t, global, "")
	if got := ChdirTarget(m, tr); got != global {
		t.Errorf("global worktree: ChdirTarget = %q, want %q", got, global)
	}

	// 4. Side-by-side worktree (repo-wt next to repo) — and its munge is a
	// near-collision with a subdir spelling; the launch cwd from the
	// transcript must win exactly.
	side := filepath.Join(root, "repo-wt")
	collider := filepath.Join(root, "repo", "wt") // munge(root/repo/wt) == munge(root/repo-wt)
	for _, d := range []string{side, collider} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if Munge(side) != Munge(collider) {
		t.Fatalf("test premise broken: %q vs %q", Munge(side), Munge(collider))
	}
	m, tr = mk(t, side, "")
	if got := ChdirTarget(m, tr); got != side {
		t.Errorf("side-by-side: ChdirTarget = %q, want %q (not the munge-colliding %q)", got, side, collider)
	}

	// 5. Launch dir deleted after saving: no chdir, resume still attempted.
	gone := filepath.Join(root, "deleted-repo")
	if err := os.MkdirAll(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	m, tr = mk(t, gone, "")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if got := ChdirTarget(m, tr); got != "" {
		t.Errorf("deleted launch dir: ChdirTarget = %q, want none", got)
	}
	var out bytes.Buffer
	// Decide still resolves to a resume (non-tty announce path).
	projects := filepath.Dir(filepath.Dir(tr))
	d := Decide(&out, root, projects, sid2, false, false, nil)
	if d.Skip || len(d.Argv) != 3 || d.Chdir != "" {
		t.Errorf("deleted launch dir: decision = %+v, want resume with no chdir", d)
	}

	// 6. Pane recreated in a different cwd: Decide's Chdir (the launch dir)
	// is authoritative — the caller chdirs regardless of where the pane is.
	m, tr = mk(t, main, "")
	if got := ChdirTarget(m, tr); got != main {
		t.Errorf("pane-cwd-independent: ChdirTarget = %q, want %q", got, main)
	}
}
