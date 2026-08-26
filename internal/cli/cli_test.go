package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var out, errb bytes.Buffer
	Version = "v0.0-test"
	if code := Run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "go-tmux-saver v0.0-test") {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestRunUnknown(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"bogus"}, &out, &errb); code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
	if !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("want usage on stderr, got %q", errb.String())
	}
}

// TestVersionFlagSpellings: --version and -v behave exactly like the
// version subcommand (the conventional spellings people type first), and
// --help/-h print usage with exit 0 rather than the unknown-command error.
func TestVersionFlagSpellings(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var out, errb bytes.Buffer
		if code := Run([]string{arg}, &out, &errb); code != 0 {
			t.Errorf("%s: exit %d, want 0 (stderr %q)", arg, code, errb.String())
		}
		if !strings.HasPrefix(out.String(), "go-tmux-saver ") {
			t.Errorf("%s: stdout = %q, want the version line", arg, out.String())
		}
	}
	for _, arg := range []string{"--help", "-h", "help"} {
		var out, errb bytes.Buffer
		if code := Run([]string{arg}, &out, &errb); code != 0 {
			t.Errorf("%s: exit %d, want 0", arg, code)
		}
		if !strings.Contains(out.String(), "usage: go-tmux-saver") {
			t.Errorf("%s: stdout = %q, want usage", arg, out.String())
		}
	}
}
