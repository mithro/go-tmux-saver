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

func TestPaneKey(t *testing.T) {
	if got := PaneKey("net", 2, 0); got != "net_2_0" {
		t.Fatal(got)
	}
	if got := PaneKey("a b/c", 1, 1); got != "a-b-c_1_1" {
		t.Fatal(got)
	}
}
