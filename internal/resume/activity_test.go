package resume

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestReadActivityModelAndLastTS: the newest real assistant model wins (never
// the "<synthetic>" placeholder), LastTS is the newest user/assistant entry,
// and later bookkeeping entries don't count as activity.
func TestReadActivityModelAndLastTS(t *testing.T) {
	projects := writeTranscript(t, "/p",
		`{"type":"assistant","timestamp":"2026-09-29T01:00:00Z","message":{"model":"claude-opus-5"}}`,
		`{"type":"assistant","timestamp":"2026-09-29T02:00:00Z","message":{"model":"claude-opus-5-5"}}`,
		`{"type":"user","timestamp":"2026-09-29T02:30:00Z","message":{"content":"tool output mentioning \"type\":\"assistant\""}}`,
		`{"type":"assistant","timestamp":"2026-09-29T03:00:00Z","message":{"model":"<synthetic>"}}`,
		`{"type":"system","timestamp":"2026-09-29T04:00:00Z"}`,
	)
	a, ok := ReadActivity(FindTranscript(projects, sid))
	if !ok || a.Model != "claude-opus-5-5" || a.LastTS != "2026-09-29T03:00:00Z" {
		t.Fatalf("ReadActivity = %+v ok=%v", a, ok)
	}
}

// TestReadActivityNoTrailingNewline: the last line is read even when the
// file doesn't end in a newline (Claude may be mid-write).
func TestReadActivityNoTrailingNewline(t *testing.T) {
	tr := FindTranscript(writeTranscript(t, "/p"), sid)
	last := `{"type":"assistant","timestamp":"2026-09-29T01:00:00Z","message":{"model":"claude-opus-5-5"}}`
	if err := os.WriteFile(tr, []byte(last), 0o600); err != nil {
		t.Fatal(err)
	}
	a, _ := ReadActivity(tr)
	if a.Model != "claude-opus-5-5" {
		t.Fatalf("Model = %q", a.Model)
	}
}

// TestReadActivityOldModelBeyondWindow: the model is found even when the
// last assistant entry is many user entries back — up to recentTurns.
func TestReadActivityOldModelBeyondWindow(t *testing.T) {
	lines := []string{`{"type":"assistant","timestamp":"2026-09-29T01:00:00Z","message":{"model":"claude-opus-5-5"}}`}
	for i := 0; i < recentTurns-1; i++ {
		lines = append(lines, `{"type":"user","timestamp":"2026-09-29T02:00:00Z"}`)
	}
	a, _ := ReadActivity(FindTranscript(writeTranscript(t, "/p", lines...), sid))
	if a.Model != "claude-opus-5-5" {
		t.Fatalf("Model = %q, want the assistant entry %d lines back", a.Model, recentTurns-1)
	}
}

// queueOp renders one transcript queue-operation entry at 2026-09-29T00:00:<sec>Z.
func queueOp(op string, sec int) string {
	return fmt.Sprintf(`{"type":"queue-operation","operation":%q,"timestamp":"2026-09-29T00:00:%02dZ"}`, op, sec)
}

// TestQueued replays the input queue: enqueue adds, dequeue (sent to the
// model) and remove (deleted) take one, popAll (queued text pulled back into
// the input box) empties it. Ops before `since` belong to an earlier process
// of the same session — the queue is in-memory, so they are ignored.
func TestQueued(t *testing.T) {
	since := time.Date(2026, 9, 29, 0, 0, 10, 0, time.UTC)
	cases := []struct {
		name string
		ops  []string
		want int
	}{
		{"one pending", []string{queueOp("enqueue", 11), queueOp("enqueue", 12), queueOp("dequeue", 13)}, 1},
		{"removed", []string{queueOp("enqueue", 11), queueOp("remove", 12)}, 0},
		{"popAll empties", []string{queueOp("enqueue", 11), queueOp("enqueue", 12), queueOp("popAll", 13)}, 0},
		{"pre-start ops ignored", []string{queueOp("enqueue", 1), queueOp("enqueue", 2), queueOp("enqueue", 11)}, 1},
		{"dequeue never negative", []string{queueOp("dequeue", 11), queueOp("enqueue", 12)}, 1},
		{"no queue", nil, 0},
	}
	for _, c := range cases {
		projects := writeTranscript(t, "/p", append([]string{`{"type":"user","timestamp":"2026-09-29T00:00:00Z"}`}, c.ops...)...)
		a, _ := ReadActivity(FindTranscript(projects, sid))
		if got := a.Queued(since); got != c.want {
			t.Errorf("%s: Queued = %d, want %d (ops %s)", c.name, got, c.want, strings.Join(c.ops, " "))
		}
		// Queue bookkeeping must not count as conversation activity.
		if a.LastTS != "2026-09-29T00:00:00Z" {
			t.Errorf("%s: LastTS = %q, want the user turn's", c.name, a.LastTS)
		}
	}
}
