package procs

import "testing"

func TestResolve(t *testing.T) {
	tb, _ := Scan("testdata/proc")
	reg := ClaudeRegistry{Dir: "testdata/sessions"}
	r := Resolve(tb, reg, 100, DefaultAllowlist) // bash → claude(registry) → tail
	if r.Kind != "claude" || r.ClaudeSession != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("registry claude: %+v", r)
	}
	r = Resolve(tb, reg, 200, DefaultAllowlist) // python3 claude-resume <uuid> placeholder
	if r.Kind != "claude" || r.ClaudeSession != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("argv claude: %+v", r)
	}
	r = Resolve(tb, reg, 300, DefaultAllowlist) // bash → ssh
	if r.Kind != "argv" || len(r.Argv) != 3 || r.Argv[0] != "ssh" {
		t.Fatalf("ssh argv: %+v", r)
	}
	r = Resolve(tb, reg, 300, nil) // nothing allowed → shell
	if r.Kind != "shell" {
		t.Fatalf("shell: %+v", r)
	}
	r = Resolve(tb, reg, 500, DefaultAllowlist) // ssh (root, no shell) → ssh argv
	if r.Kind != "argv" || len(r.Argv) != 2 || r.Argv[0] != "ssh" {
		t.Fatalf("ssh root argv: %+v", r)
	}
	r = Resolve(tb, reg, 501, DefaultAllowlist) // foo-claude-resume (bad boundary) → shell
	if r.Kind != "shell" {
		t.Fatalf("bad boundary: %+v", r)
	}
	if r := Resolve(tb, reg, 4242, DefaultAllowlist); r.Kind != "shell" {
		t.Fatalf("unknown pid: %+v", r)
	}
}

// TestResolveStaleRegistryFallsThrough covers the issue-#8 gap: a claude
// process whose registry entry is STALE (procStart no longer matches — the
// pid was recycled or the file outlived the process) must not stop
// resolution; rules 2–4 still run, and here rule 2 (--resume on the live
// cmdline) recovers the real session id.
func TestResolveStaleRegistryFallsThrough(t *testing.T) {
	tb, _ := Scan("testdata/proc")
	reg := ClaudeRegistry{Dir: "testdata/sessions"}
	r := Resolve(tb, reg, 600, DefaultAllowlist) // bash → claude (stale 601.json, live --resume)
	if r.Kind != "claude" || r.ClaudeSession != "bbbbbbbb-cccc-dddd-eeee-ffffffffffff" {
		t.Fatalf("stale registry should fall through to the cmdline --resume rule: %+v", r)
	}
}
