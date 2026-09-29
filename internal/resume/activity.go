package resume

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

// Activity is what a status check needs from a transcript. ReadActivity
// gets it without JSON-decoding every line — transcripts reach 100+ MB and
// ReadMeta's full decode costs seconds each.
type Activity struct {
	// Model is the model of the newest real assistant message ("" if none).
	Model string
	// LastTS is the timestamp of the newest user/assistant entry — the
	// conversation's last activity. (Not the file's mtime: Remote Control
	// and hook bookkeeping append to idle sessions.)
	LastTS string
	// Queue is the transcript's input-queue log, oldest first (see Queued).
	Queue []QueueOp
}

// QueueOp is one `queue-operation` transcript entry: Claude Code logs its
// in-memory input queue — messages that arrived while it couldn't take them
// (mid-turn, or a dialog open) — as enqueue / dequeue / remove / popAll.
type QueueOp struct {
	Op string
	TS time.Time
}

// Queued is how many messages are waiting in the input queue: enqueue adds
// one, dequeue (sent to the model) and remove (deleted) take one, popAll
// (queued text pulled back into the input box for editing) empties it. Only
// ops at or after since — the running process's start — count: the queue is
// in-memory, so a --resume starts empty while the transcript keeps the
// previous process's unmatched enqueues. Prompt suggestions never enter it.
func (a Activity) Queued(since time.Time) int {
	n := 0
	for _, q := range a.Queue {
		if q.TS.Before(since) {
			continue
		}
		switch q.Op {
		case "enqueue":
			n++
		case "dequeue", "remove":
			n = max(0, n-1)
		case "popAll":
			n = 0
		}
	}
	return n
}

// recentTurns bounds how many trailing user/assistant lines ReadActivity
// keeps for the final decode; an assistant entry (carrying the model) is
// always among the last few.
const recentTurns = 64

// ReadActivity scans a transcript cheaply: queue-operation lines (small)
// are decoded as they come; user/assistant lines are only kept raw, and just
// the newest are decoded at the end. A byte match of `"type":"user"` can't
// come from nested content — inside a JSON string its quotes are escaped —
// and every candidate is re-checked after decoding anyway.
func ReadActivity(path string) (Activity, bool) {
	var a Activity
	f, err := os.Open(path)
	if err != nil {
		return a, false
	}
	defer f.Close()
	var recent [][]byte
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		switch {
		case bytes.Contains(line, []byte(`"queue-operation"`)):
			var o struct {
				Type, Operation, Timestamp string
			}
			if json.Unmarshal(line, &o) == nil && o.Type == "queue-operation" {
				if ts, perr := time.Parse(time.RFC3339Nano, o.Timestamp); perr == nil {
					a.Queue = append(a.Queue, QueueOp{Op: o.Operation, TS: ts})
				}
			}
		case bytes.Contains(line, []byte(`"type":"user"`)) || bytes.Contains(line, []byte(`"type":"assistant"`)):
			if len(recent) == recentTurns {
				recent = recent[1:]
			}
			recent = append(recent, line)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return a, false
		}
	}
	for i := len(recent) - 1; i >= 0; i-- {
		var o struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if json.Unmarshal(recent[i], &o) != nil || o.Type != "user" && o.Type != "assistant" {
			continue
		}
		if a.LastTS == "" {
			a.LastTS = o.Timestamp
		}
		if o.Type == "assistant" && o.Message.Model != "" && o.Message.Model != "<synthetic>" {
			a.Model = o.Message.Model
			break
		}
	}
	return a, true
}
