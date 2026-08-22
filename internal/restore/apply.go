package restore

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

// Report summarises what Apply did against a live tmux server.
type Report struct {
	Created, Relocated, Skipped int
	Notes                       []string
}

// Apply runs a Plan's Actions against t. WinPlaceholder ({{WIN}}) in a
// relocated window's actions is substituted with the index reported by that
// window's "new-window ... -P -F" reply, and stays in effect (unchanged)
// until the next relocation resolves it to a new value — see BuildPlan's doc
// comment for why every use of one relocation's placeholder is contiguous in
// the action list.
//
// "contents" actions replay saved pane scrollback by writing it to a private
// temp file, `load-buffer`-ing it into a dedicated tmux buffer ("gts") and
// `paste-buffer`-ing that into the target pane (no shell involvement), then
// removing the temp file.
//
// An error from an ordinary action is appended to Report.Notes and does not
// stop the rest of the plan — a later re-run of an up-to-date plan (built
// from the now-current live state) completes whatever didn't apply. The one
// exception is a failed session/window-creating command (new-session,
// new-window): since nothing downstream in that window's block can possibly
// succeed against a window that doesn't exist (and, for a relocation, its
// WinPlaceholder can't even be resolved), the remaining actions of that
// block are skipped — processing resumes at the next new-session/new-window
// action, so unrelated sessions/windows are unaffected.
func Apply(ctx context.Context, t tmuxctl.Transport, p Plan, contents func(paneKey string) ([]byte, bool)) (Report, error) {
	report := Report{Created: p.Created, Relocated: p.Relocated, Skipped: p.Skipped}

	var winTarget string // most recently resolved WinPlaceholder value, once a relocation has replied
	aborted := false     // true while skipping the remainder of a failed creation's block

	resolve := func(s string) string {
		if winTarget == "" {
			return s
		}
		return strings.ReplaceAll(s, WinPlaceholder, winTarget)
	}

	for _, a := range p.Actions {
		switch a.Kind {
		case "note":
			// No side effect (e.g. "skipped" for an already-matching window).

		case "tmux":
			cmd := a.Args[0]
			create := isCreateCmd(cmd)
			if create {
				aborted = false // a new window/session block starts here
			} else if aborted {
				continue
			}

			resolved := resolve(cmd)
			lines, err := t.Run(ctx, resolved)
			if err != nil {
				report.Notes = append(report.Notes, fmt.Sprintf("%s: %v", resolved, err))
				if create {
					aborted = true
				}
				continue
			}
			if a.Note == "relocated" {
				idx, ok := firstLine(lines)
				if !ok {
					report.Notes = append(report.Notes, fmt.Sprintf("%s: no window index in reply", resolved))
					aborted = true
					continue
				}
				winTarget = idx
			}

		case "contents":
			if aborted {
				continue
			}
			target := resolve(a.Args[0])
			paneKey := a.Args[1]
			data, ok := contents(paneKey)
			if !ok {
				continue // nothing saved for this pane; not an error
			}
			if err := replayContents(ctx, t, target, data); err != nil {
				report.Notes = append(report.Notes, fmt.Sprintf("contents %s: %v", target, err))
			}
		}
	}

	return report, nil
}

func isCreateCmd(cmd string) bool {
	return strings.HasPrefix(cmd, "new-session ") || strings.HasPrefix(cmd, "new-window ")
}

// firstLine returns the first non-blank line of lines, verified to parse as
// an integer (tmux's "#{window_index}" reply).
func firstLine(lines []string) (string, bool) {
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if _, err := strconv.Atoi(l); err != nil {
			return "", false
		}
		return l, true
	}
	return "", false
}

// replayContents pastes data into target via a private temp file, tmux's
// load-buffer/paste-buffer (no shell involvement), removing the temp file
// once done.
func replayContents(ctx context.Context, t tmuxctl.Transport, target string, data []byte) error {
	f, err := os.CreateTemp("", "gts-restore-*.bin")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}

	if _, err := t.Run(ctx, fmt.Sprintf("load-buffer -b gts %s", tmpPath)); err != nil {
		return err
	}
	if _, err := t.Run(ctx, fmt.Sprintf("paste-buffer -d -b gts -t %s", target)); err != nil {
		return err
	}
	return nil
}
