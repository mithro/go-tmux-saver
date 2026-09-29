package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRCState(t *testing.T) {
	cases := []struct {
		name   string
		screen []string
		want   string
	}{
		{"connected", []string{"x", "   \x1b[38;5;114m/rc\x1b[39m", ""}, "connected"},
		{"failed", []string{"   \x1b[38;5;211m/rc failed\x1b[39m"}, "failed"},
		{"connecting", []string{"   \x1b[2m/rc\x1b[0m"}, "connecting"},
		{"off", []string{"no indicator"}, "off"},
		// Only the bottom lines are the status area: a /rc typed higher up
		// the screen must not count.
		{"above status area", []string{"❯ /rc", "1", "2", "3", "4", "5", "6"}, "off"},
		// Trailing blank lines don't push the indicator out of the window.
		{"trailing blanks", []string{"a", "b", "\x1b[38;5;114m/rc\x1b[39m", "", "", "", "", "", ""}, "connected"},
		{"blank", nil, "off"},
	}
	for _, c := range cases {
		if got := rcState(c.screen); got != c.want {
			t.Errorf("%s: rcState = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestModelLabel(t *testing.T) {
	for in, want := range map[string]string{
		"claude-opus-5-5":           "Opus 5.5",
		"claude-haiku-4-5-20251001": "Haiku 4.5",
		"claude-fable-5":            "Fable 5",
		"claude-opus-4-8":           "Opus 4.8",
		"some-other-model":          "some-other-model",
		"":                          "?",
	} {
		if got := modelLabel(in); got != want {
			t.Errorf("modelLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgo(t *testing.T) {
	for d, want := range map[time.Duration]string{
		5 * time.Second:               "5s",
		125 * time.Second:             "2m",
		2*time.Hour + time.Minute:     "2h01m",
		55*time.Hour + 20*time.Minute: "2d07h",
		-3 * time.Second:              "0s", // clock skew vs a just-written turn
	} {
		if got := ago(d); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestLatestVersion(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"2.1.9", "2.1.280", "2.1.28"} {
		if err := os.WriteFile(filepath.Join(dir, v), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := latestVersion(dir); got != "2.1.280" {
		t.Fatalf("latestVersion = %q", got)
	}
	if got := latestVersion(filepath.Join(dir, "missing")); got != "" {
		t.Fatalf("missing dir: latestVersion = %q, want \"\"", got)
	}
}

// statusFixture: pane %5 in default:1 runs claude pid 101 (session file
// fixture: rcfiles-work, idle, 2.1.284, started 2026-09-29T05:58:34Z), with
// Remote Control connected and a transcript holding one pending message.
func statusFixture(t *testing.T) (SuspendDeps, *strings.Builder, func() []string) {
	t.Helper()
	f := suspendFake()
	f.Replies["capture-pane -e -p -t \"%5\""] = append(append([]string{}, screenEmptyPrompt...), "  \x1b[38;5;114m/rc\x1b[39m")
	d, out := suspendFixture(t, f, false)
	d.ProjectsDir = writeTranscript(t,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-09-29T01:00:00Z"}`, // earlier process: moot
		`{"type":"assistant","cwd":"/home/u/proj","timestamp":"2026-09-29T09:00:00Z","message":{"model":"claude-opus-5-5"}}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-09-29T10:00:00Z"}`,
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-09-29T10:00:01Z"}`,
		`{"type":"queue-operation","operation":"dequeue","timestamp":"2026-09-29T10:00:02Z"}`,
	)
	d.Now = func() time.Time { return idleNow } // 12:00 ⇒ idle 3h
	d.VersionsDir = t.TempDir()
	for _, v := range []string{"2.1.284", "2.1.290"} {
		if err := os.WriteFile(filepath.Join(d.VersionsDir, v), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return d, out, func() []string { return f.Calls }
}

// TestRunClaudeStatus: one row with every column, a summary line, and not a
// single key sent — status is read-only.
func TestRunClaudeStatus(t *testing.T) {
	d, out, calls := statusFixture(t)
	if err := RunClaudeStatus(context.Background(), d, "default", "1", false, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want header, row, blank, summary; got:\n%s", out.String())
	}
	for _, col := range []string{"window", "name", "status", "rc", "version", "model", "queued", "idle"} {
		if !strings.Contains(lines[0], col) {
			t.Errorf("header %q missing %q", lines[0], col)
		}
	}
	if strings.Contains(lines[0], "resume") {
		t.Errorf("resume column must be opt-in: %q", lines[0])
	}
	if got := strings.Fields(lines[1]); strings.Join(got, " ") != "default:1 rcfiles-work idle connected 2.1.284 OLD Opus 5.5 1 3h00m" {
		t.Errorf("row = %q", lines[1])
	}
	if want := "1 sessions; latest installed claude 2.1.290 (1 older); remote control not connected: 0; with queued messages: 1"; lines[3] != want {
		t.Errorf("summary = %q\nwant      %q", lines[3], want)
	}
	for _, c := range calls() {
		if strings.HasPrefix(c, "send-keys") {
			t.Fatalf("status must not send keys: %q", c)
		}
	}
}

// TestRunClaudeStatusShowResume: the opt-in column carries a runnable command.
func TestRunClaudeStatusShowResume(t *testing.T) {
	d, out, _ := statusFixture(t)
	if err := RunClaudeStatus(context.Background(), d, "default", "1", false, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "cd /home/u/proj && claude --resume "+suspendSID) {
		t.Fatalf("output = %q", out.String())
	}
}

// TestRunClaudeStatusNoTranscript: a never-messaged session still gets a row,
// with unknowns shown as "?".
func TestRunClaudeStatusNoTranscript(t *testing.T) {
	d, out, _ := statusFixture(t)
	d.ProjectsDir = t.TempDir()
	if err := RunClaudeStatus(context.Background(), d, "default", "1", false, false); err != nil {
		t.Fatal(err)
	}
	row := strings.Split(out.String(), "\n")[1]
	if got := strings.Join(strings.Fields(row), " "); got != "default:1 rcfiles-work idle connected 2.1.284 OLD ? ? ?" {
		t.Fatalf("row = %q", row)
	}
}

func TestShQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/home/tim/local":     "/home/tim/local",
		"/home/tim/my dir":    "'/home/tim/my dir'",
		"/tmp/it's":           `'/tmp/it'\''s'`,
		"/a/.claude/wt/x-y_1": "/a/.claude/wt/x-y_1",
	} {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
