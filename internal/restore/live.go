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
	liveWinCmd     = `list-windows -a -F "#{session_name}\t#{window_index}\t#{window_name}\t#{session_grouped}\t#{session_group}"`
	liveClientsCmd = `list-clients -F "#{client_name}"`
)

// QueryLive gathers the live session/window layout and the attached-client
// count from a running tmux server. Grouped sessions keep exactly ONE
// canonical member (issue #12): every member of a tmux session group reports
// session_grouped=1 — there is no "original" flagged 0 — so excluding all
// grouped rows made whole groups invisible and a restore would recreate
// windows that already exist live. tmuxctl.CanonicalMember picks the
// survivor per #{session_group}.
func QueryLive(ctx context.Context, t tmuxctl.Transport) (LiveState, error) {
	live := LiveState{Sessions: map[string][]LiveWindow{}}

	winLines, err := t.Run(ctx, liveWinCmd)
	if err != nil {
		return LiveState{}, fmt.Errorf("list-windows: %w", err)
	}
	// Pass 1: parse every row, bucketing grouped sessions by group name so
	// pass 2 can keep exactly one canonical member per group.
	type winRow struct {
		session, name  string
		winIdx         int
		grouped, group string
	}
	var rows []winRow
	groups := map[string]map[string]bool{}
	for _, l := range winLines {
		if l == "" {
			continue
		}
		// tail = session_grouped, session_group (peeled from the right so
		// window_name, to their left, safely absorbs any embedded tab);
		// head is then split left-to-right into exactly 3 fields.
		idx := strings.LastIndexByte(l, '\t')
		if idx < 0 {
			return LiveState{}, fmt.Errorf("malformed list-windows line: %q", l)
		}
		rest, group := l[:idx], l[idx+1:]
		idx = strings.LastIndexByte(rest, '\t')
		if idx < 0 {
			return LiveState{}, fmt.Errorf("malformed list-windows line: %q", l)
		}
		head, grouped := rest[:idx], rest[idx+1:]
		f := strings.SplitN(head, "\t", 3)
		if len(f) != 3 {
			return LiveState{}, fmt.Errorf("malformed list-windows line: %q", l)
		}
		session, winIdxStr, name := f[0], f[1], f[2]
		winIdx, err := strconv.Atoi(winIdxStr)
		if err != nil {
			return LiveState{}, fmt.Errorf("malformed list-windows line (window_index): %q", l)
		}
		rows = append(rows, winRow{session: session, name: name, winIdx: winIdx, grouped: grouped, group: group})
		if grouped == "1" {
			if groups[group] == nil {
				groups[group] = map[string]bool{}
			}
			groups[group][session] = true
		}
	}
	canonical := map[string]string{}
	for g, memberSet := range groups {
		members := make([]string, 0, len(memberSet))
		for m := range memberSet {
			members = append(members, m)
		}
		canonical[g] = tmuxctl.CanonicalMember(g, members)
	}
	for _, r := range rows {
		if r.grouped == "1" && r.session != canonical[r.group] {
			continue // non-canonical group member: same windows, skip
		}
		live.Sessions[r.session] = append(live.Sessions[r.session], LiveWindow{Index: r.winIdx, Name: r.name})
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
