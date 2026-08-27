package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

// TestClaudeResumeCLIExecsClaude drives the subcommand with a swapped exec:
// non-tty stdin/stdout (as in a send-keys restore) must resolve straight to
// exec'ing `claude --resume <sid>` without blocking for a keypress.
func TestClaudeResumeCLIExecsClaude(t *testing.T) {
	var got []string
	execveFn = func(path string, argv []string, env []string) error {
		got = argv
		return nil
	}
	lookPathFn = func(name string) (string, error) { return "/stub/" + name, nil }
	t.Cleanup(func() { execveFn, lookPathFn = nil, nil }) // no test may exec for real

	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	var out, errb bytes.Buffer
	code := Run([]string{"claude-resume", sid}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if len(got) != 3 || got[0] != "claude" || got[1] != "--resume" || got[2] != sid {
		t.Fatalf("exec argv = %q", got)
	}
	if !strings.Contains(out.String(), "will still try to resume") {
		t.Fatalf("banner = %q", out.String())
	}
}

// TestClaudeResumeCLIJunkSidPicker: junk id → plain `claude` picker.
func TestClaudeResumeCLIJunkSidPicker(t *testing.T) {
	var got []string
	execveFn = func(path string, argv []string, env []string) error {
		got = argv
		return nil
	}
	lookPathFn = func(name string) (string, error) { return "/stub/" + name, nil }
	t.Cleanup(func() { execveFn, lookPathFn = nil, nil })

	var out, errb bytes.Buffer
	if code := Run([]string{"claude-resume", "junk"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(got) != 1 || got[0] != "claude" {
		t.Fatalf("exec argv = %q", got)
	}
}

// TestRunMultiCallClaudeResume: invoked via a `claude-resume` symlink
// (argv0 basename), the binary IS the placeholder — args pass through as
// the subcommand's own.
func TestRunMultiCallClaudeResume(t *testing.T) {
	var got []string
	execveFn = func(path string, argv []string, env []string) error {
		got = argv
		return nil
	}
	lookPathFn = func(name string) (string, error) { return "/stub/" + name, nil }
	t.Cleanup(func() { execveFn, lookPathFn = nil, nil })

	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	var out, errb bytes.Buffer
	code := RunMultiCall("/home/tim/bin/claude-resume", []string{sid}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if len(got) != 3 || got[0] != "claude" || got[2] != sid {
		t.Fatalf("exec argv = %q", got)
	}
	// The ordinary name still dispatches subcommands normally.
	out.Reset()
	if code := RunMultiCall("/usr/bin/go-tmux-saver", []string{"version"}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "go-tmux-saver ") {
		t.Fatalf("normal dispatch broken: %d %q", code, out.String())
	}
}

// TestClaudeResumeSavedOutputFile covers issue #15: --saved-output prints
// the file's content ABOVE the banner (the pane's pre-suspend console
// state), and flags are accepted after the positional session id too — the
// generated command lines keep the id directly after `claude-resume` so
// /proc re-detection still matches.
func TestClaudeResumeSavedOutputFile(t *testing.T) {
	execveFn = func(path string, argv []string, env []string) error { return nil }
	lookPathFn = func(name string) (string, error) { return "/stub/" + name, nil }
	t.Cleanup(func() { execveFn, lookPathFn = nil, nil })

	saved := filepath.Join(t.TempDir(), "saved.txt")
	if err := os.WriteFile(saved, []byte("previous console output\nlast line before suspend\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	for _, args := range [][]string{
		{"claude-resume", "--saved-output", saved, sid},
		{"claude-resume", sid, "--saved-output", saved}, // positional-first (generated form)
	} {
		var out, errb bytes.Buffer
		if code := Run(args, &out, &errb); code != 0 {
			t.Fatalf("%v: exit %d; stderr=%q", args, code, errb.String())
		}
		s := out.String()
		iOut, iBanner := strings.Index(s, "last line before suspend"), strings.Index(s, "Resume Claude session")
		if iOut < 0 || iBanner < 0 || iOut > iBanner {
			t.Fatalf("%v: saved output must precede the banner:\n%s", args, s)
		}
	}
}

// TestClaudeResumeSavedOutputFromStore: without --saved-output, the
// placeholder looks the pane up in the store's last snapshot by session id
// and prints (the tail of) its saved scrollback; --no-saved suppresses it.
func TestClaudeResumeSavedOutputFromStore(t *testing.T) {
	execveFn = func(path string, argv []string, env []string) error { return nil }
	lookPathFn = func(name string) (string, error) { return "/stub/" + name, nil }
	t.Cleanup(func() { execveFn, lookPathFn = nil, nil })

	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	dataDir := t.TempDir()
	gz, _ := snapshot.LookupCodec("gzip")
	store := &snapshot.Store{Dir: dataDir, Codec: gz}
	if err := store.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	snap := &snapshot.Snapshot{Schema: snapshot.SchemaVersion, TakenAt: time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC),
		Sessions: []snapshot.Session{{Name: "s", Windows: []snapshot.Window{{Index: 0, Name: "w",
			Panes: []snapshot.Pane{{Index: 0, Restore: snapshot.Restore{Kind: "claude", ClaudeSession: sid}}}}}}}}
	stg, err := store.Stage(snap, map[string][]byte{"s_0_0": []byte("store line one\nstore line two\n")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stg.Promote(); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeConfig(t, "{}")

	var out, errb bytes.Buffer
	code := Run([]string{"claude-resume", "--config", cfgPath, "--data-dir", dataDir, sid}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "store line two") {
		t.Fatalf("saved store output missing:\n%s", out.String())
	}
	if i, j := strings.Index(out.String(), "store line two"), strings.Index(out.String(), "Resume Claude session"); i > j {
		t.Fatalf("saved output must precede the banner:\n%s", out.String())
	}

	out.Reset()
	if code := Run([]string{"claude-resume", "--config", cfgPath, "--data-dir", dataDir, "--no-saved", sid}, &out, &errb); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out.String(), "store line") {
		t.Fatalf("--no-saved must suppress the saved output:\n%s", out.String())
	}
}
