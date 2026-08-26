package restore

import (
	"fmt"
	"os"
	"strings"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
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
//     pane's saved content by paneKey and displays it in paneTarget.
//   - "note": Note is a log line with no side effect (e.g. "skipped").
//
// Session names the saved session this action belongs to. Every action for
// one saved session is contiguous in Plan.Actions (BuildPlan's outer loop is
// per-session), so Apply can use Session to recognise "everything left in
// this session's block" after a new-session failure, without having to parse
// session names back out of tmux command text.
type Action struct {
	Kind    string
	Args    []string
	Note    string
	Session string
}

// Plan is the ordered list of Actions BuildPlan produces to bring a live
// tmux server into the shape recorded by a snapshot.
type Plan struct {
	Actions   []Action
	Created   int
	Relocated int
	Skipped   int
}

func (p *Plan) tmux(session, cmd, note string) {
	p.Actions = append(p.Actions, Action{Kind: "tmux", Args: []string{cmd}, Note: note, Session: session})
}

func (p *Plan) note(session, msg string) {
	p.Actions = append(p.Actions, Action{Kind: "note", Note: msg, Session: session})
}

// warn records a message the applier must surface in Report.Notes (unlike
// the count-only "note" kind) — used for whole-session refusals (issue #5).
func (p *Plan) warn(session, msg string) {
	p.Actions = append(p.Actions, Action{Kind: "warn", Note: msg, Session: session})
}

func (p *Plan) contents(session, paneTarget, paneKey string) {
	p.Actions = append(p.Actions, Action{Kind: "contents", Args: []string{paneTarget, paneKey}, Session: session})
}

// findWindowByName returns the index of the live window named name, if any.
func findWindowByName(liveWins []LiveWindow, name string) (int, bool) {
	for _, lw := range liveWins {
		if lw.Name == name {
			return lw.Index, true
		}
	}
	return 0, false
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

// tmuxQuote wraps s as ONE tmux double-quoted command-line argument — see
// tmuxctl.Quote for the escaping rules (RULING R30) and for why EVERY
// data-derived argument, composed "-t sess:idx[.pane]" targets included,
// must go through it.
func tmuxQuote(s string) string { return tmuxctl.Quote(s) }

// cwdArg renders the " -c <dir>" fragment for a window/pane creation
// command, or "" when there is no cwd at all (a saved window with no
// panes) — an empty `-c ""` would otherwise ask tmux to chdir to nowhere.
func cwdArg(cwd string) string {
	if cwd == "" {
		return ""
	}
	return " -c " + tmuxQuote(cwd)
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
		// Issue #5, probe-verified on tmux 3.5a: a session name containing
		// ':' cannot be addressed by ANY target spelling — "a:b:1" parses as
		// window "b:1", and the exact-match form "=a:b:1" as session "a" —
		// so every post-creation action would fail. Refuse the whole
		// session loudly rather than half-restore it. ('.' names are fine:
		// the full "sess:idx" form splits at the colon first.)
		if strings.Contains(sess.Name, ":") {
			plan.warn(sess.Name, fmt.Sprintf("session %q skipped: tmux targets cannot address a session name containing ':' (%d windows not restored)", sess.Name, len(sess.Windows)))
			for range sess.Windows {
				plan.Skipped++
				plan.note(sess.Name, "skipped: unaddressable session name")
			}
			continue
		}

		liveWins, sessExists := live.Sessions[sess.Name]
		liveByIdx := map[int]string{}
		for _, lw := range liveWins {
			liveByIdx[lw.Index] = lw.Name
		}
		sessionCreated := !sessExists

		// Relocations run AFTER every fixed-index creation in the session:
		// a relocating new-window takes tmux's next-free index at apply
		// time, so emitted earlier it could steal an index a later saved
		// window owns (seen live on ten64: seed h at 0 forced saved bash:0
		// to relocate, tmux picked index 1, and tmux-restore:1 then failed
		// "index 1 in use"). willRelocate mirrors the decision switch in
		// the loop body — side-effect free.
		willRelocate := func(i int, win snapshot.Window) bool {
			if sessionCreated && i == 0 {
				return false
			}
			liveName, occ := liveByIdx[win.Index]
			if !occ || liveName == win.Name {
				return false
			}
			if _, ok := findWindowByName(liveWins, win.Name); ok {
				return false
			}
			return true
		}
		var order []int
		for i, win := range sess.Windows {
			if !willRelocate(i, win) {
				order = append(order, i)
			}
		}
		for i, win := range sess.Windows {
			if willRelocate(i, win) {
				order = append(order, i)
			}
		}

		for _, i := range order {
			win := sess.Windows[i]
			var target string
			var relocated bool
			created := false
			cwd0 := firstCwd(win.Panes)

			switch {
			// Name encoding (probe-verified, 3.5a vs next-3.8): tmux
			// vis(3)-encodes SESSION names at creation on both versions, so
			// the -s VALUE is decoded (tmuxctl.UnvisName) and tmux's own
			// re-encoding reproduces the snapshot's stored spelling. WINDOW
			// -n values store raw on 3.5a but encoded on next-3.8 — the one
			// version-stable primitive is rename-window (encodes on both),
			// so names containing a backslash get a follow-up rename-window
			// below and -n stays verbatim. -t TARGETS always use the stored
			// spelling — target matching compares it verbatim.
			case sessionCreated && i == 0:
				target = fmt.Sprintf("=%s:%d", sess.Name, win.Index)
				created = true
				plan.tmux(sess.Name, fmt.Sprintf("new-session -d -s %s -n %s%s", tmuxQuote(tmuxctl.UnvisName(sess.Name)), tmuxQuote(win.Name), cwdArg(cwd0)), "")
			default:
				liveName, occ := liveByIdx[win.Index]
				switch {
				case !occ:
					target = fmt.Sprintf("=%s:%d", sess.Name, win.Index)
					created = true
					plan.tmux(sess.Name, fmt.Sprintf("new-window -d -t %s -n %s%s", tmuxQuote(target), tmuxQuote(win.Name), cwdArg(cwd0)), "")
				case liveName == win.Name:
					plan.Skipped++
					plan.note(sess.Name, fmt.Sprintf("%s:%d %q already present", sess.Name, win.Index, win.Name))
					continue
				default:
					// RULING R27: don't relocate a same-named window that
					// already exists live at some OTHER index — re-relocating
					// it on every run (since the saved index stays occupied
					// by the foreign window forever) is not idempotent.
					if idx, ok := findWindowByName(liveWins, win.Name); ok {
						plan.Skipped++
						plan.note(sess.Name, fmt.Sprintf("%s %q present at index %d", sess.Name, win.Name, idx))
						continue
					}
					target = fmt.Sprintf("=%s:%s", sess.Name, WinPlaceholder)
					created = true
					relocated = true
					plan.tmux(sess.Name, fmt.Sprintf(`new-window -d -P -F "#{window_index}" -t %s -n %s%s`, tmuxQuote("="+sess.Name+":"), tmuxQuote(win.Name), cwdArg(cwd0)), "relocated")
				}
			}

			if created && strings.Contains(win.Name, `\\`) {
				// See the name-encoding comment above: a DOUBLED backslash
				// marks a vis-encoded stored name, and only rename-window
				// reproduces that encoding on every supported tmux version.
				// (A lone backslash is a raw-stored 3.5a-era name: -n
				// verbatim is exact there, and no command can store it on an
				// encoding version, so no rename is emitted for those.)
				plan.tmux(sess.Name, fmt.Sprintf("rename-window -t %s %s", tmuxQuote(target), tmuxQuote(tmuxctl.UnvisName(win.Name))), "")
			}
			for k := 1; k < len(win.Panes); k++ {
				plan.tmux(sess.Name, fmt.Sprintf("split-window -d -t %s%s", tmuxQuote(target), cwdArg(cwdOrHome(win.Panes[k].Cwd))), "")
			}
			plan.tmux(sess.Name, fmt.Sprintf("select-layout -t %s %s", tmuxQuote(target), tmuxQuote(win.Layout)), "")

			activePane := 0
			for _, pn := range win.Panes {
				if pn.Active {
					activePane = pn.Index
				}
			}
			for _, pn := range win.Panes {
				paneTarget := fmt.Sprintf("%s.%d", target, pn.Index)
				if o.Contents && pn.ContentFile != "" {
					plan.contents(sess.Name, paneTarget, snapshot.PaneKey(sess.Name, win.Index, pn.Index))
				}
				switch pn.Restore.Kind {
				case "argv":
					if len(pn.Restore.Argv) > 0 { // empty argv: treated like "shell", no send-keys
						// shellQuote's result must reach tmux as ONE key
						// argument, not spliced in as bare, space-separated
						// tokens on the tmux command line: tmux's send-keys types
						// each of its own arguments back-to-back with NO space
						// inserted between them, so unquoted multi-token argv
						// (e.g. 'tail' '-f' '/dev/null') would be typed into the
						// pane as "tail-f/dev/null". tmuxQuote (RULING R30, not
						// Go's %q — tmux's own double-quote syntax expands $NAME
						// and mangles \xNN) wraps it as one literal argument,
						// spaces included, for the pane's own shell to re-parse
						// on Enter — exactly as shellQuote intends.
						plan.tmux(sess.Name, fmt.Sprintf("send-keys -t %s %s Enter", tmuxQuote(paneTarget), tmuxQuote(shellQuote(pn.Restore.Argv))), "")
					}
				case "claude":
					plan.tmux(sess.Name, fmt.Sprintf("send-keys -t %s %s Enter", tmuxQuote(paneTarget), tmuxQuote(shellQuote([]string{o.ClaudeResumePath, pn.Restore.ClaudeSession}))), "")
				}
			}
			plan.tmux(sess.Name, fmt.Sprintf("select-pane -t %s", tmuxQuote(fmt.Sprintf("%s.%d", target, activePane))), "")
			if !win.AutomaticRename {
				plan.tmux(sess.Name, fmt.Sprintf("set-window-option -t %s automatic-rename off", tmuxQuote(target)), "")
			}

			if relocated {
				plan.Relocated++
			} else if created {
				plan.Created++
			}
			if created && win.Index == sess.ActiveWindow {
				plan.tmux(sess.Name, fmt.Sprintf("select-window -t %s", tmuxQuote(target)), "")
			}
		}
	}

	return plan
}
