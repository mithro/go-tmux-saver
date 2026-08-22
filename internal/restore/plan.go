package restore

import (
	"fmt"
	"os"
	"strings"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

// WinPlaceholder marks a relocated window's real index within an Action's
// tmux command line. Apply (Task 14) resolves it from the reply of that
// window's "new-window ... -P -F "#{window_index}"" command.
const WinPlaceholder = "{{WIN}}"

// Options configures how BuildPlan restores a snapshot onto a live server.
type Options struct {
	ClaudeResumePath string
	Contents         bool
	SeedSession      string
	SeedWindow       string
}

// Action is one step of a Plan. Kind is one of:
//   - "tmux": Args[0] is one full tmux command line.
//   - "contents": Args = [paneTarget, paneKey]; the applier looks up the
//     pane's saved content by paneKey and pastes it into paneTarget.
//   - "note": Note is a log line with no side effect (e.g. "skipped").
type Action struct {
	Kind string
	Args []string
	Note string
}

// Plan is the ordered list of Actions BuildPlan produces to bring a live
// tmux server into the shape recorded by a snapshot.
type Plan struct {
	Actions   []Action
	Created   int
	Relocated int
	Skipped   int
}

func (p *Plan) tmux(cmd, note string) {
	p.Actions = append(p.Actions, Action{Kind: "tmux", Args: []string{cmd}, Note: note})
}

func (p *Plan) note(msg string) {
	p.Actions = append(p.Actions, Action{Kind: "note", Note: msg})
}

func (p *Plan) contents(paneTarget, paneKey string) {
	p.Actions = append(p.Actions, Action{Kind: "contents", Args: []string{paneTarget, paneKey}})
}

// shellQuote renders argv as a space-joined, single-quoted argument string
// safe to splice into a tmux send-keys command line.
func shellQuote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}

// cwdOrHome returns cwd if it still names an existing directory, else the
// user's home directory — used for split-window panes whose saved cwd may
// no longer exist.
func cwdOrHome(cwd string) string {
	if fi, err := os.Stat(cwd); err == nil && fi.IsDir() {
		return cwd
	}
	h, _ := os.UserHomeDir()
	return h
}

// firstCwd returns cwdOrHome(panes[0].Cwd), or "" if there are no panes.
// Every pane's cwd — including the first pane used on the
// new-session/new-window line — goes through the same missing-directory
// fallback: a new-session/new-window with a nonexistent -c fails in tmux
// just as surely as a split-window would.
func firstCwd(panes []snapshot.Pane) string {
	if len(panes) == 0 {
		return ""
	}
	return cwdOrHome(panes[0].Cwd)
}

// BuildPlan is a pure function: given the live server state, a saved
// snapshot, and restore options, it returns the ordered Actions that graft
// every saved session/window not already present live onto the running
// server.
//
// For each saved window the live state decides what happens:
//   - the session doesn't exist yet: the window is created (the session's
//     first window via new-session, the rest via new-window at their saved
//     index — nothing live can conflict with a session that doesn't exist).
//   - the saved index is free live: new-window at that index.
//   - the saved index is occupied live by a window of the same name: left
//     entirely alone (this is how the seed session/window, and any
//     already-restored windows, are never touched — no tmux action is
//     emitted for it at all).
//   - the saved index is occupied live by a window of a different name:
//     relocated — new-window lets tmux pick the next free index, and every
//     subsequent action for that window addresses it via the WinPlaceholder
//     target, which Apply resolves at run time.
//
// select-window (when the session was created, or this window was
// created/relocated and it is sess.ActiveWindow) is emitted immediately at
// the end of that window's own iteration, never deferred to the end of the
// session's loop: WinPlaceholder is a single fixed token, so if a session
// relocates more than one window, a deferred select-window referencing
// "{{WIN}}" would end up resolved against whichever relocation happened to
// run last, not the one it was meant for. Emitting it inline keeps every
// use of a given relocation's "{{WIN}}" contiguous in the action list, so
// Apply's sequential placeholder substitution is unambiguous.
func BuildPlan(live LiveState, snap *snapshot.Snapshot, o Options) Plan {
	var plan Plan

	for _, sess := range snap.Sessions {
		liveWins, sessExists := live.Sessions[sess.Name]
		liveByIdx := map[int]string{}
		for _, lw := range liveWins {
			liveByIdx[lw.Index] = lw.Name
		}
		sessionCreated := !sessExists

		for i, win := range sess.Windows {
			var target string
			var relocated bool
			created := false
			cwd0 := firstCwd(win.Panes)

			switch {
			case sessionCreated && i == 0:
				target = fmt.Sprintf("%s:%d", sess.Name, win.Index)
				created = true
				plan.tmux(fmt.Sprintf("new-session -d -s %s -n %s -c %s", sess.Name, win.Name, cwd0), "")
			default:
				liveName, occ := liveByIdx[win.Index]
				switch {
				case !occ:
					target = fmt.Sprintf("%s:%d", sess.Name, win.Index)
					created = true
					plan.tmux(fmt.Sprintf("new-window -d -t %s:%d -n %s -c %s", sess.Name, win.Index, win.Name, cwd0), "")
				case liveName == win.Name:
					plan.Skipped++
					plan.note("skipped")
					continue
				default:
					target = fmt.Sprintf("%s:%s", sess.Name, WinPlaceholder)
					created = true
					relocated = true
					plan.tmux(fmt.Sprintf(`new-window -d -P -F "#{window_index}" -t %s: -n %s -c %s`, sess.Name, win.Name, cwd0), "relocated")
				}
			}

			for k := 1; k < len(win.Panes); k++ {
				plan.tmux(fmt.Sprintf("split-window -d -t %s -c %s", target, cwdOrHome(win.Panes[k].Cwd)), "")
			}
			plan.tmux(fmt.Sprintf(`select-layout -t %s "%s"`, target, win.Layout), "")

			activePane := 0
			for _, pn := range win.Panes {
				if pn.Active {
					activePane = pn.Index
				}
			}
			for _, pn := range win.Panes {
				paneTarget := fmt.Sprintf("%s.%d", target, pn.Index)
				if o.Contents && pn.ContentFile != "" {
					plan.contents(paneTarget, snapshot.PaneKey(sess.Name, win.Index, pn.Index))
				}
				switch pn.Restore.Kind {
				case "argv":
					if len(pn.Restore.Argv) > 0 { // empty argv: treated like "shell", no send-keys
						plan.tmux(fmt.Sprintf("send-keys -t %s %s Enter", paneTarget, shellQuote(pn.Restore.Argv)), "")
					}
				case "claude":
					plan.tmux(fmt.Sprintf("send-keys -t %s %s Enter", paneTarget, shellQuote([]string{o.ClaudeResumePath, pn.Restore.ClaudeSession})), "")
				}
			}
			plan.tmux(fmt.Sprintf("select-pane -t %s.%d", target, activePane), "")
			if !win.AutomaticRename {
				plan.tmux(fmt.Sprintf("set-window-option -t %s automatic-rename off", target), "")
			}

			if relocated {
				plan.Relocated++
			} else if created {
				plan.Created++
			}
			if created && win.Index == sess.ActiveWindow {
				plan.tmux(fmt.Sprintf("select-window -t %s", target), "")
			}
		}
	}

	return plan
}
