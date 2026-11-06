package snapshot

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSchemaRoundTrip(t *testing.T) {
	s := &Snapshot{Schema: SchemaVersion, Host: "h", TakenAt: time.Unix(1, 0).UTC(),
		Sessions: []Session{{Name: "net", Windows: []Window{{Index: 2, Name: "w",
			Panes: []Pane{{Index: 0, ID: "%5", Cwd: "/tmp", Restore: Restore{Kind: "argv", Argv: []string{"ssh", "host"}}}}}}}}}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back Snapshot
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Sessions[0].Windows[0].Panes[0].Restore.Argv[1] != "host" || back.Schema != 1 {
		t.Fatalf("round trip lost data: %+v", back)
	}
	p, w := back.CountPanes()
	if p != 1 || w != 1 {
		t.Fatalf("counts %d %d", p, w)
	}
}

// TestSchemaDeadPaneRoundTrip covers issue #55: a dead pane's exit status
// must survive the round trip, INCLUDING a status of 0 (which is why it is a
// pointer — a plain int with omitempty would drop it), and an absent
// status/signal must stay absent rather than becoming 0.
func TestSchemaDeadPaneRoundTrip(t *testing.T) {
	zero, nine := 0, 9
	in := []Restore{
		{Kind: KindDead, ExitStatus: &zero},
		{Kind: KindDead, ExitSignal: &nine},
		{Kind: KindDead},
	}
	for _, r := range in {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var back Restore
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if back.Kind != KindDead || !intPtrEqual(back.ExitStatus, r.ExitStatus) || !intPtrEqual(back.ExitSignal, r.ExitSignal) {
			t.Fatalf("round trip %s -> %+v, want %+v", b, back, r)
		}
	}
}

func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestPaneKey(t *testing.T) {
	if got := PaneKey("net", 2, 0); got != "net_2_0" {
		t.Fatal(got)
	}
	if got := PaneKey("a b/c", 1, 1); got != "a-b-c_1_1" {
		t.Fatal(got)
	}
}
