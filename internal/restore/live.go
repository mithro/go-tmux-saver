// Package restore turns a saved snapshot into the ordered tmux commands
// needed to graft it onto a live server, without ever touching a window
// that already matches what was saved.
package restore

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

// LiveWindow is a single window observed on a running tmux server.
type LiveWindow struct {
	Index int
	Name  string
}

// LiveState is the current shape of a running tmux server: session name ->
// its non-grouped windows, plus the number of attached clients.
type LiveState struct {
	Sessions map[string][]LiveWindow
	Clients  int
}

const (
	liveWinCmd     = `list-windows -a -F "#{session_name}\t#{window_index}\t#{window_name}\t#{session_grouped}"`
	liveClientsCmd = `list-clients -F "#{client_name}"`
)

// QueryLive gathers the live session/window layout (grouped-session clones
// excluded) and the attached-client count from a running tmux server.
func QueryLive(ctx context.Context, t tmuxctl.Transport) (LiveState, error) {
	live := LiveState{Sessions: map[string][]LiveWindow{}}

	winLines, err := t.Run(ctx, liveWinCmd)
	if err != nil {
		return LiveState{}, fmt.Errorf("list-windows: %w", err)
	}
	for _, l := range winLines {
		if l == "" {
			continue
		}
		// tail = session_grouped (peeled from the right so window_name, to
		// its left, safely absorbs any embedded tab); head is then split
		// left-to-right into exactly 3 fields.
		idx := strings.LastIndexByte(l, '\t')
		if idx < 0 {
			return LiveState{}, fmt.Errorf("malformed list-windows line: %q", l)
		}
		head, grouped := l[:idx], l[idx+1:]
		if grouped == "1" {
			continue // grouped clone
		}
		f := strings.SplitN(head, "\t", 3)
		if len(f) != 3 {
			return LiveState{}, fmt.Errorf("malformed list-windows line: %q", l)
		}
		session, winIdxStr, name := f[0], f[1], f[2]
		winIdx, err := strconv.Atoi(winIdxStr)
		if err != nil {
			return LiveState{}, fmt.Errorf("malformed list-windows line (window_index): %q", l)
		}
		live.Sessions[session] = append(live.Sessions[session], LiveWindow{Index: winIdx, Name: name})
	}

	clientLines, err := t.Run(ctx, liveClientsCmd)
	if err != nil {
		return LiveState{}, fmt.Errorf("list-clients: %w", err)
	}
	for _, l := range clientLines {
		if l != "" {
			live.Clients++
		}
	}

	return live, nil
}

// IsSeedOnly reports whether l consists of exactly one session, named
// seedSession, containing exactly one window, named seedWindow — i.e. the
// server has not been touched beyond the always-present seed shell.
func IsSeedOnly(l LiveState, seedSession, seedWindow string) bool {
	if len(l.Sessions) != 1 {
		return false
	}
	wins, ok := l.Sessions[seedSession]
	if !ok {
		return false
	}
	return len(wins) == 1 && wins[0].Name == seedWindow
}
