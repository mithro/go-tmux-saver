// Package collect builds a snapshot from a live tmux server.
package collect

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mithro/go-tmux-saver/internal/procs"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

const (
	SessCmd   = "list-sessions -F \"#{session_name}\t#{session_grouped}\t#{session_attached}\""
	WinCmd    = "list-windows -a -F \"#{session_name}\t#{window_index}\t#{window_name}\t#{window_active}\t#{window_flags}\t#{window_layout}\t#{automatic-rename}\""
	PaneCmd   = "list-panes -a -F \"#{session_name}\t#{window_index}\t#{pane_index}\t#{pane_id}\t#{pane_active}\t#{pane_pid}\t#{pane_current_path}\t#{pane_title}\t#{history_size}\""
	ServerCmd = "display-message -p \"#{start_time}\t#{version}\t#{client_session}\""
)

// Collector builds a snapshot.Snapshot and per-pane contents by issuing
// list-sessions/list-windows/list-panes/capture-pane over a single
// tmuxctl.Transport (a live control-mode connection or a Fake in tests).
type Collector struct {
	T         tmuxctl.Transport
	Procs     *procs.Table
	Reg       procs.ClaudeRegistry
	Allowlist []string
	Host      string
	Now       func() time.Time
}

func (c *Collector) Collect(ctx context.Context) (*snapshot.Snapshot, map[string][]byte, error) {
	start := time.Now()
	now := c.Now
	if now == nil {
		now = time.Now
	}
	snap := &snapshot.Snapshot{Schema: snapshot.SchemaVersion, Host: c.Host, TakenAt: now().UTC()}

	lines, err := c.T.Run(ctx, ServerCmd)
	if err != nil {
		return nil, nil, fmt.Errorf("server info: %w", err)
	}
	if len(lines) == 1 {
		f := strings.Split(lines[0], "\t")
		if len(f) == 3 {
			snap.ServerStart, _ = strconv.ParseInt(f[0], 10, 64)
			snap.TmuxVersion = f[1]
			snap.Client.Session = f[2]
		}
	}

	sessLines, err := c.T.Run(ctx, SessCmd)
	if err != nil {
		return nil, nil, fmt.Errorf("list-sessions: %w", err)
	}
	sessions := map[string]*snapshot.Session{}
	var order []string
	for _, l := range sessLines {
		f := strings.Split(l, "\t")
		if len(f) < 2 || f[1] == "1" {
			continue // grouped clone
		}
		sessions[f[0]] = &snapshot.Session{Name: f[0]}
		order = append(order, f[0])
	}
	sort.Strings(order)

	winLines, err := c.T.Run(ctx, WinCmd)
	if err != nil {
		return nil, nil, fmt.Errorf("list-windows: %w", err)
	}
	// Pass 1: append every window to its session, recording where it landed.
	winPos := map[string]int{} // "session\tindex" -> position in se.Windows
	for _, l := range winLines {
		f := strings.Split(l, "\t")
		if len(f) < 7 {
			continue
		}
		se, ok := sessions[f[0]]
		if !ok {
			continue // grouped clone or unknown session
		}
		idx, _ := strconv.Atoi(f[1])
		w := snapshot.Window{Index: idx, Name: f[2], Active: f[3] == "1", Flags: f[4], Layout: f[5], AutomaticRename: f[6] == "on"}
		if w.Active {
			se.ActiveWindow = idx
		}
		se.Windows = append(se.Windows, w)
		winPos[f[0]+"\t"+f[1]] = len(se.Windows) - 1
	}

	paneLines, err := c.T.Run(ctx, PaneCmd)
	if err != nil {
		return nil, nil, fmt.Errorf("list-panes: %w", err)
	}
	type capture struct {
		key, id string
		hist    int
	}
	var caps []capture
	// Pass 2: no further appends happen to any se.Windows slice, so taking
	// &se.Windows[pos] here is safe and stable for the rest of Collect.
	for _, l := range paneLines {
		f := strings.Split(l, "\t")
		if len(f) < 9 {
			continue
		}
		se, ok := sessions[f[0]]
		if !ok {
			continue // grouped clone or unknown session
		}
		pos, ok := winPos[f[0]+"\t"+f[1]]
		if !ok {
			continue
		}
		w := &se.Windows[pos]
		idx, _ := strconv.Atoi(f[2])
		pid, _ := strconv.Atoi(f[5])
		hist, _ := strconv.Atoi(f[8])
		p := snapshot.Pane{
			Index:        idx,
			ID:           f[3],
			Active:       f[4] == "1",
			Cwd:          f[6],
			Title:        f[7],
			HistoryLines: hist,
			Restore:      procs.Resolve(c.Procs, c.Reg, pid, c.Allowlist),
		}
		w.Panes = append(w.Panes, p)
		caps = append(caps, capture{snapshot.PaneKey(f[0], w.Index, idx), f[3], hist})
	}

	contents := map[string][]byte{}
	for _, cp := range caps {
		lines, err := c.T.Run(ctx, fmt.Sprintf("capture-pane -epJ -S -%d -t %s", cp.hist, cp.id))
		if err != nil {
			return nil, nil, fmt.Errorf("capture %s: %w", cp.id, err)
		}
		contents[cp.key] = []byte(strings.Join(lines, "\n") + "\n")
	}

	for _, name := range order {
		se := sessions[name]
		sort.Slice(se.Windows, func(i, j int) bool { return se.Windows[i].Index < se.Windows[j].Index })
		for i := range se.Windows {
			ps := se.Windows[i].Panes
			sort.Slice(ps, func(a, b int) bool { return ps[a].Index < ps[b].Index })
		}
		snap.Sessions = append(snap.Sessions, *se)
	}

	p, w := snap.CountPanes()
	snap.Stats = snapshot.Stats{Panes: p, Windows: w, Sessions: len(snap.Sessions), DurationMS: time.Since(start).Milliseconds()}
	return snap, contents, nil
}
