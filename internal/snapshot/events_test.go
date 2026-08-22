package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEventsAppendTailFresh(t *testing.T) {
	dir := t.TempDir()
	if _, ok, _ := LastGood(dir); ok {
		t.Fatal("no marker yet")
	}
	e := Event{Time: time.Date(2026, 8, 22, 1, 2, 3, 0, time.UTC), Outcome: "kept", Panes: 46, Windows: 40,
		Sessions: 6, Clients: 2, DurationMS: 310, File: "snap-x", Detail: ""}
	if err := AppendEvent(dir, e); err != nil {
		t.Fatal(err)
	}
	AppendEvent(dir, Event{Time: e.Time.Add(time.Minute), Outcome: "rejected-degenerate", Panes: 1, Detail: "1 vs 46"})
	got, err := TailEvents(dir, 5)
	if err != nil || len(got) != 2 {
		t.Fatalf("tail = %+v %v", got, err)
	}
	if got[0].Outcome != "kept" || got[0].Panes != 46 || got[0].DurationMS != 310 || got[1].Detail != "1 vs 46" {
		t.Fatalf("parsed %+v", got)
	}
	if err := TouchFresh(dir); err != nil {
		t.Fatal(err)
	}
	if ts, ok, _ := LastGood(dir); !ok || time.Since(ts) > time.Minute {
		t.Fatalf("fresh marker %v %v", ts, ok)
	}
}

func TestEventFieldSanitization(t *testing.T) {
	dir := t.TempDir()
	e := Event{
		Time:       time.Date(2026, 8, 22, 1, 2, 3, 0, time.UTC),
		Outcome:    "test",
		Panes:      1,
		Windows:    1,
		Sessions:   1,
		Clients:    1,
		DurationMS: 100,
		File:       "x\ty",
		Detail:     "a\tb\nc",
	}
	if err := AppendEvent(dir, e); err != nil {
		t.Fatal(err)
	}
	// Verify parsing: Detail and File should have tabs/newlines replaced with spaces
	got, err := TailEvents(dir, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("TailEvents failed: %v", err)
	}
	if got[0].File != "x y" {
		t.Errorf("File: got %q, want %q", got[0].File, "x y")
	}
	if got[0].Detail != "a b c" {
		t.Errorf("Detail: got %q, want %q", got[0].Detail, "a b c")
	}
	// Verify raw line has exactly 9 tab-separated fields (8 tabs)
	raw, err := os.ReadFile(filepath.Join(dir, eventsFile))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSuffix(string(raw), "\n")
	tabCount := strings.Count(line, "\t")
	if tabCount != 8 {
		t.Errorf("line has %d tabs (want 8): %q", tabCount, line)
	}
	fieldCount := strings.Count(line, "\t") + 1
	if fieldCount != 9 {
		t.Errorf("line has %d tab-separated fields (want 9)", fieldCount)
	}
}
