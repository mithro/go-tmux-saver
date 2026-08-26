package cli

import (
	"bytes"
	"strings"
	"testing"
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
	t.Cleanup(func() { execveFn = nil }) // no test may exec for real

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
	t.Cleanup(func() { execveFn = nil })

	var out, errb bytes.Buffer
	if code := Run([]string{"claude-resume", "junk"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(got) != 1 || got[0] != "claude" {
		t.Fatalf("exec argv = %q", got)
	}
}
