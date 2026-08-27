package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// linkEnv builds an Env managing <tmp>/bin/claude-resume with a fake
// installed binary at <tmp>/usr/go-tmux-saver, returning env and both paths.
func linkEnv(t *testing.T) (Env, string, string) {
	t.Helper()
	root := t.TempDir()
	binary := filepath.Join(root, "usr", "go-tmux-saver")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("fake-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "bin", "claude-resume")
	return Env{ClaudeResumeLink: link, Binary: binary}, link, binary
}

func mustEnsure(t *testing.T, env Env) (bool, string) {
	t.Helper()
	changed, note, err := EnsureClaudeResumeLink(env, false)
	if err != nil {
		t.Fatal(err)
	}
	return changed, note
}

func assertPointsAt(t *testing.T, link, binary string) {
	t.Helper()
	dest, err := os.Readlink(link)
	if err != nil || dest != binary {
		t.Fatalf("link -> %q err=%v, want %q", dest, err, binary)
	}
}

func TestClaudeResumeLinkCreatedWhenAbsent(t *testing.T) {
	env, link, binary := linkEnv(t)
	changed, note := mustEnsure(t, env)
	if !changed || !strings.Contains(note, "created") {
		t.Fatalf("changed=%v note=%q", changed, note)
	}
	assertPointsAt(t, link, binary)
	// Second run: stable no-op.
	if changed, note := mustEnsure(t, env); changed || !strings.Contains(note, "ok") {
		t.Fatalf("second run changed=%v note=%q", changed, note)
	}
	if _, ok := ClaudeResumeDrift(env); ok {
		t.Fatal("no drift expected once the link is correct")
	}
}

func TestClaudeResumeLinkReplacesBrokenSymlink(t *testing.T) {
	env, link, binary := linkEnv(t)
	os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), link); err != nil {
		t.Fatal(err)
	}
	if d, ok := ClaudeResumeDrift(env); !ok || !strings.Contains(d.Diff, "broken") {
		t.Fatalf("drift = %+v ok=%v, want broken-symlink drift", d, ok)
	}
	changed, note := mustEnsure(t, env)
	if !changed || !strings.Contains(note, "broken symlink") {
		t.Fatalf("changed=%v note=%q", changed, note)
	}
	assertPointsAt(t, link, binary)
}

func TestClaudeResumeLinkReplacesOldBinarySymlink(t *testing.T) {
	env, link, binary := linkEnv(t)
	old := filepath.Join(t.TempDir(), "go-tmux-saver")
	if err := os.WriteFile(old, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.Symlink(old, link); err != nil {
		t.Fatal(err)
	}
	changed, note := mustEnsure(t, env)
	if !changed || !strings.Contains(note, "old go-tmux-saver") {
		t.Fatalf("changed=%v note=%q", changed, note)
	}
	assertPointsAt(t, link, binary)
}

func TestClaudeResumeLinkReplacesKnownScript(t *testing.T) {
	// Register a synthetic "known" script checksum for the test.
	script := []byte("#!/usr/bin/env python3\n# synthetic claude-resume\n")
	sum := sha256.Sum256(script)
	key := hex.EncodeToString(sum[:])
	knownClaudeResumeSHA256[key] = true
	t.Cleanup(func() { delete(knownClaudeResumeSHA256, key) })

	// Plain file with the known checksum → replaced.
	env, link, binary := linkEnv(t)
	os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.WriteFile(link, script, 0o755); err != nil {
		t.Fatal(err)
	}
	changed, note := mustEnsure(t, env)
	if !changed || !strings.Contains(note, "known claude-resume script") {
		t.Fatalf("changed=%v note=%q", changed, note)
	}
	assertPointsAt(t, link, binary)

	// Symlink resolving to the known script (the rcfiles deployment shape)
	// → the LINK is re-pointed; the script file itself is untouched.
	env2, link2, binary2 := linkEnv(t)
	scriptPath := filepath.Join(t.TempDir(), "claude-resume")
	if err := os.WriteFile(scriptPath, script, 0o755); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(link2), 0o755)
	if err := os.Symlink(scriptPath, link2); err != nil {
		t.Fatal(err)
	}
	changed, note = mustEnsure(t, env2)
	if !changed || !strings.Contains(note, "known claude-resume script") {
		t.Fatalf("changed=%v note=%q", changed, note)
	}
	assertPointsAt(t, link2, binary2)
	if got, err := os.ReadFile(scriptPath); err != nil || string(got) != string(script) {
		t.Fatalf("the known script's own file must be untouched: %q %v", got, err)
	}
}

func TestClaudeResumeLinkLeavesForeignAlone(t *testing.T) {
	// Unknown plain file.
	env, link, _ := linkEnv(t)
	os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.WriteFile(link, []byte("#!/bin/sh\nmy own tool\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	changed, note := mustEnsure(t, env)
	if changed || !strings.Contains(note, "left unchanged") {
		t.Fatalf("changed=%v note=%q", changed, note)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("foreign file must remain a plain file")
	}
	if _, ok := ClaudeResumeDrift(env); ok {
		t.Fatal("foreign item must NOT be drift (validate would fail forever)")
	}

	// Symlink to a different, working tool.
	env2, link2, _ := linkEnv(t)
	other := filepath.Join(t.TempDir(), "sometool")
	if err := os.WriteFile(other, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(link2), 0o755)
	if err := os.Symlink(other, link2); err != nil {
		t.Fatal(err)
	}
	changed, note = mustEnsure(t, env2)
	if changed || !strings.Contains(note, "different tool") {
		t.Fatalf("changed=%v note=%q", changed, note)
	}
	if dest, _ := os.Readlink(link2); dest != other {
		t.Fatalf("foreign symlink re-pointed to %q", dest)
	}
}

func TestClaudeResumeLinkDryRun(t *testing.T) {
	env, link, _ := linkEnv(t)
	changed, note, err := EnsureClaudeResumeLink(env, true)
	if err != nil || !changed || !strings.Contains(note, "would have created") {
		t.Fatalf("changed=%v note=%q err=%v", changed, note, err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("dry run must not create the link")
	}
}

func TestClaudeResumeLinkUnconfiguredIsNoop(t *testing.T) {
	changed, _, err := EnsureClaudeResumeLink(Env{}, false)
	if err != nil || changed {
		t.Fatalf("unconfigured env: changed=%v err=%v", changed, err)
	}
}

// TestInstallAndUpdateManageTheLink: Install creates the link as part of
// its normal flow, and Update repairs a broken one even when every managed
// FILE is already up to date (the early no-file-change return must not skip
// the link check).
func TestInstallAndUpdateManageTheLink(t *testing.T) {
	env, link, binary := linkEnv(t)
	env.ConfigHome = t.TempDir()
	env.Systemctl = func(args ...string) (string, error) { return "", nil }
	var out strings.Builder
	env.Stdout = &out

	files, err := Render(Params{Version: "vtest", Binary: binary, Socket: "s", SeedSession: "default", SeedWindow: "h", IntervalMinutes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := Install(env, files); err != nil {
		t.Fatal(err)
	}
	assertPointsAt(t, link, binary)
	if !strings.Contains(out.String(), "created") {
		t.Fatalf("install output = %q, want the created note", out.String())
	}

	// Break the link; files stay current → Update must still repair it.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), link); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	changed, err := Update(env, files, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("no managed files should change, got %v", changed)
	}
	assertPointsAt(t, link, binary)
	if !strings.Contains(out.String(), "replaced") {
		t.Fatalf("update output = %q, want the replaced note", out.String())
	}
}
