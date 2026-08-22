package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPruneCLIDryRunDoesNotRemove covers `prune --dry-run`: it must report
// what would be removed without touching the filesystem, while a real
// `prune` run (against the same retention policy) actually removes it.
func TestPruneCLIDryRunDoesNotRemove(t *testing.T) {
	dataDir := t.TempDir()
	old := "snap-20200101T000000Z"
	newest := "snap-20260822T000000Z"
	if err := os.MkdirAll(filepath.Join(dataDir, old), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, newest), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(newest, filepath.Join(dataDir, "last")); err != nil {
		t.Fatal(err)
	}

	// keep=1, daily_days=0, rejected=0 so only `newest` (the `last` target)
	// survives — old is outside every retention window.
	cfgPath := writeConfig(t, `{"retention": {"keep": 1, "daily_days": 0, "rejected": 0}}`)

	var out, errb bytes.Buffer
	code := Run([]string{"prune", "--dry-run", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("dry-run exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), old) {
		t.Fatalf("dry-run stdout = %q, want it to mention %q", out.String(), old)
	}
	if _, err := os.Stat(filepath.Join(dataDir, old)); err != nil {
		t.Fatalf("--dry-run must not remove anything, but %s: %v", old, err)
	}

	out.Reset()
	code = Run([]string{"prune", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("prune exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), old) {
		t.Fatalf("prune stdout = %q, want it to mention %q", out.String(), old)
	}
	if _, err := os.Stat(filepath.Join(dataDir, old)); !os.IsNotExist(err) {
		t.Fatalf("prune should have removed %s, stat err = %v", old, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, newest)); err != nil {
		t.Fatalf("prune must keep the `last` target %s: %v", newest, err)
	}
}

// TestPruneCLINothingToPrune covers the empty-retention-window case: an
// empty data dir has nothing to remove, and prune must say so rather than
// print a blank list.
func TestPruneCLINothingToPrune(t *testing.T) {
	dataDir := t.TempDir()
	cfgPath := writeConfig(t, "{}")

	var out, errb bytes.Buffer
	code := Run([]string{"prune", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "nothing to prune") {
		t.Fatalf("stdout = %q, want %q", out.String(), "nothing to prune")
	}
}
