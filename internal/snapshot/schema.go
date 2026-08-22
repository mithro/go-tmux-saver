package snapshot

import (
	"fmt"
	"regexp"
	"time"
)

const SchemaVersion = 1

type Snapshot struct {
	Schema        int         `json:"schema"`
	Host          string      `json:"host"`
	TmuxVersion   string      `json:"tmux_version"`
	TakenAt       time.Time   `json:"taken_at"`
	ServerStart   int64       `json:"server_start"`
	ContentsCodec string      `json:"contents_codec"`
	Sessions      []Session   `json:"sessions"`
	Client        ClientState `json:"client"`
	Stats         Stats       `json:"stats"`
}

type Session struct {
	Name         string   `json:"name"`
	ActiveWindow int      `json:"active_window"`
	Windows      []Window `json:"windows"`
}

type Window struct {
	Index           int    `json:"index"`
	Name            string `json:"name"`
	Layout          string `json:"layout"`
	Active          bool   `json:"active"`
	Flags           string `json:"flags"`
	AutomaticRename bool   `json:"automatic_rename"`
	Panes           []Pane `json:"panes"`
}

type Pane struct {
	Index         int     `json:"index"`
	ID            string  `json:"id"`
	Cwd           string  `json:"cwd"`
	Title         string  `json:"title"`
	Active        bool    `json:"active"`
	HistoryLines  int     `json:"history_lines"`
	ContentSHA256 string  `json:"content_sha256,omitempty"`
	ContentFile   string  `json:"content_file,omitempty"`
	Restore       Restore `json:"restore"`
}

type Restore struct {
	Kind          string   `json:"kind"`
	Argv          []string `json:"argv,omitempty"`
	ClaudeSession string   `json:"claude_session,omitempty"`
}

type ClientState struct {
	Session string `json:"session"`
}

type Stats struct {
	Panes      int   `json:"panes"`
	Windows    int   `json:"windows"`
	Sessions   int   `json:"sessions"`
	DurationMS int64 `json:"duration_ms"`
}

var keyUnsafe = regexp.MustCompile(`[\s/]+`)

// PaneKey is the structural, server-restart-stable name of a pane.
func PaneKey(session string, window, pane int) string {
	return fmt.Sprintf("%s_%d_%d", keyUnsafe.ReplaceAllString(session, "-"), window, pane)
}

func (s *Snapshot) CountPanes() (panes, windows int) {
	for _, se := range s.Sessions {
		windows += len(se.Windows)
		for _, w := range se.Windows {
			panes += len(w.Panes)
		}
	}
	return
}
