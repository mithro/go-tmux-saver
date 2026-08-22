package snapshot

import (
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
