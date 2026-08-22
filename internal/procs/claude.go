package procs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

// ClaudeRegistry reads Claude Code's per-pid session files
// (~/.claude/sessions/<pid>.json).
type ClaudeRegistry struct{ Dir string }

type registryEntry struct {
	SessionID string          `json:"sessionId"`
	ProcStart json.RawMessage `json:"procStart"`
}

// SessionFor returns the session id recorded for p, validated against the
// process start time so a reused pid cannot match a stale entry.
func (r ClaudeRegistry) SessionFor(p Proc) (string, bool) {
	data, err := os.ReadFile(filepath.Join(r.Dir, strconv.Itoa(p.PID)+".json"))
	if err != nil {
		return "", false
	}
	var e registryEntry
	if json.Unmarshal(data, &e) != nil || e.SessionID == "" {
		return "", false
	}
	if len(e.ProcStart) > 0 {
		var asStr string
		var asNum json.Number
		switch {
		case json.Unmarshal(e.ProcStart, &asStr) == nil:
			if asStr != p.StartTime {
				return "", false
			}
		case json.Unmarshal(e.ProcStart, &asNum) == nil:
			if asNum.String() != p.StartTime {
				return "", false
			}
		default:
			// procStart is present but wrong type (e.g. bool, array, object)
			return "", false
		}
	}
	return e.SessionID, true
}
