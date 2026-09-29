package procs

import (
	"testing"
	"time"
)

func TestScanAndSubtree(t *testing.T) {
	tb, err := Scan("testdata/proc")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := tb.Get(101)
	if !ok || p.Comm != "claude" || p.PPID != 100 || p.StartTime != "6000" {
		t.Fatalf("proc 101 = %+v ok=%v", p, ok)
	}
	p200, _ := tb.Get(200)
	if len(p200.Cmdline) != 3 || p200.Cmdline[1] != "/home/tim/bin/claude-resume" {
		t.Fatalf("cmdline = %q", p200.Cmdline)
	}
	got := tb.Subtree(100)
	want := []int{100, 101, 102}
	if len(got) != 3 || got[0] != 100 || got[1] != 101 || got[2] != 102 {
		t.Fatalf("subtree = %v want %v", got, want)
	}
}

func TestEmbeddedParenInComm(t *testing.T) {
	tb, err := Scan("testdata/proc")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := tb.Get(400)
	if !ok || p.Comm != "a (b) c" || p.PPID != 1 || p.StartTime != "9000" {
		t.Fatalf("proc 400 = %+v ok=%v", p, ok)
	}
}

func TestClaudeRegistry(t *testing.T) {
	reg := ClaudeRegistry{Dir: "testdata/sessions"}
	tb, _ := Scan("testdata/proc")
	p, _ := tb.Get(101)
	sid, ok := reg.SessionFor(p)
	if !ok || sid != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("got %q %v", sid, ok)
	}
	p.StartTime = "9999" // pid reused by a different process
	if _, ok := reg.SessionFor(p); ok {
		t.Fatal("stale procStart must not match")
	}
	if _, ok := reg.SessionFor(Proc{PID: 777}); ok {
		t.Fatal("missing registry file must not match")
	}
}

// TestClaudeRegistryEntry: the full session record, behind the same
// procStart check as SessionFor.
func TestClaudeRegistryEntry(t *testing.T) {
	reg := ClaudeRegistry{Dir: "testdata/sessions"}
	tb, _ := Scan("testdata/proc")
	p, _ := tb.Get(101)
	e, ok := reg.Entry(p)
	want := ClaudeSession{
		SessionID: "11111111-2222-3333-4444-555555555555", Name: "rcfiles-work",
		Status: "idle", Version: "2.1.284", Cwd: "/home/u/proj",
		StartedAt: time.UnixMilli(1790661514767),
	}
	if !ok || e.SessionID != want.SessionID || e.Name != want.Name || e.Status != want.Status ||
		e.Version != want.Version || e.Cwd != want.Cwd || !e.StartedAt.Equal(want.StartedAt) {
		t.Fatalf("Entry = %+v ok=%v, want %+v", e, ok, want)
	}
	p.StartTime = "9999"
	if _, ok := reg.Entry(p); ok {
		t.Fatal("stale procStart must not match")
	}
}

func TestNumericProcStart(t *testing.T) {
	reg := ClaudeRegistry{Dir: "testdata/sessions"}
	tb, _ := Scan("testdata/proc")
	p, _ := tb.Get(200)
	sid, ok := reg.SessionFor(p)
	if !ok || sid != "22222222-3333-4444-5555-666666666666" {
		t.Fatalf("got %q %v", sid, ok)
	}
	// Verify it doesn't match with different starttime
	p.StartTime = "8001"
	if _, ok := reg.SessionFor(p); ok {
		t.Fatal("numeric procStart with wrong starttime must not match")
	}
}

func TestWrongTypedProcStart(t *testing.T) {
	reg := ClaudeRegistry{Dir: "testdata/sessions"}
	tb, _ := Scan("testdata/proc")
	p, _ := tb.Get(102)
	// ProcStart is JSON boolean (true), should fail closed
	if _, ok := reg.SessionFor(p); ok {
		t.Fatal("wrong-typed procStart (boolean) must not match")
	}
}
