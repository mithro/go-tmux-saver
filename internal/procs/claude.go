package procs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// ClaudeRegistry reads Claude Code's per-pid session files
// (~/.claude/sessions/<pid>.json).
type ClaudeRegistry struct{ Dir string }

type registryEntry struct {
	SessionID string          `json:"sessionId"`
	ProcStart json.RawMessage `json:"procStart"`
	Name      string          `json:"name"`
	Status    string          `json:"status"`
	Version   string          `json:"version"`
	Cwd       string          `json:"cwd"`
	StartedAt int64           `json:"startedAt"` // unix ms
}

// ClaudeSession is one running Claude process's session file.
type ClaudeSession struct {
	SessionID string
	// Name is the session's display name (derived or user-set).
	Name string
	// Status is Claude Code's own state: idle, busy, waiting (a dialog or
	// permission prompt is open), shell, ...
	Status string
	// Version is the Claude Code version the process runs.
	Version string
	// Cwd is the launch directory.
	Cwd string
	// StartedAt is when this process started (zero if unrecorded).
	StartedAt time.Time
}

// SessionFor returns the session id recorded for p, validated against the
// process start time so a reused pid cannot match a stale entry.
func (r ClaudeRegistry) SessionFor(p Proc) (string, bool) {
	e, ok := r.Entry(p)
	return e.SessionID, ok
}

// Entry returns p's whole session file, behind the same procStart check as
// SessionFor.
func (r ClaudeRegistry) Entry(p Proc) (ClaudeSession, bool) {
	data, err := os.ReadFile(filepath.Join(r.Dir, strconv.Itoa(p.PID)+".json"))
	if err != nil {
		return ClaudeSession{}, false
	}
	var e registryEntry
	if json.Unmarshal(data, &e) != nil || e.SessionID == "" {
		return ClaudeSession{}, false
	}
	if len(e.ProcStart) > 0 {
		var asStr string
		var asNum json.Number
		switch {
		case json.Unmarshal(e.ProcStart, &asStr) == nil:
			if asStr != p.StartTime {
				return ClaudeSession{}, false
			}
		case json.Unmarshal(e.ProcStart, &asNum) == nil:
			if asNum.String() != p.StartTime {
				return ClaudeSession{}, false
			}
		default:
			// procStart is present but wrong type (e.g. bool, array, object)
			return ClaudeSession{}, false
		}
	}
	s := ClaudeSession{SessionID: e.SessionID, Name: e.Name, Status: e.Status, Version: e.Version, Cwd: e.Cwd}
	if e.StartedAt > 0 {
		s.StartedAt = time.UnixMilli(e.StartedAt)
	}
	return s, true
}
