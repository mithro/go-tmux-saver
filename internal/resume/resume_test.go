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

func TestTailLines(t *testing.T) {
	data := []byte("a\nb\nc\nd\n")
	if got := string(TailLines(data, 2)); got != "c\nd\n" {
		t.Fatalf("TailLines(2) = %q", got)
	}
	if got := string(TailLines(data, 0)); got != "a\nb\nc\nd\n" {
		t.Fatalf("TailLines(0) = %q (0 = everything)", got)
	}
	if got := string(TailLines(data, 10)); got != "a\nb\nc\nd\n" {
		t.Fatalf("TailLines(10) = %q", got)
	}
	if got := string(TailLines([]byte("no-trailing-newline"), 1)); got != "no-trailing-newline" {
		t.Fatalf("TailLines(no-nl) = %q", got)
	}
}
