// Package tmuxctl talks to a tmux server over a single control-mode connection.
package tmuxctl

import (
	"bufio"
	"io"
	"strings"
)

// Reply is the body of one %begin…%end / %error block.
type Reply struct {
	Lines []string
	Err   bool
}

// ParseReplies reads control-mode output from r and sends one Reply per
// command block on out. Notifications outside a block (%exit, %layout-change…)
// are ignored; a %session-changed/%window-* notification inside a block is
// dropped from the body. Terminators are matched by block number so a pane
// line that merely looks like "%end …" cannot close the block early.
// Returns nil at %exit or EOF.
func ParseReplies(r io.Reader, out chan<- Reply) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var (
		inBlock bool
		num     string
		cur     Reply
	)
	for sc.Scan() {
		line := sc.Text()
		if !inBlock {
			if strings.HasPrefix(line, "%begin ") {
				f := strings.Fields(line)
				if len(f) >= 3 {
					inBlock, num, cur = true, f[2], Reply{}
				}
				continue
			}
			if line == "%exit" {
				return nil
			}
			continue // other notification
		}
		if strings.HasPrefix(line, "%end ") || strings.HasPrefix(line, "%error ") {
			f := strings.Fields(line)
			if len(f) >= 3 && f[2] == num {
				cur.Err = f[0] == "%error"
				out <- cur
				inBlock = false
				continue
			}
		}
		if strings.HasPrefix(line, "%") && isNotification(line) {
			continue
		}
		cur.Lines = append(cur.Lines, line)
	}
	return sc.Err()
}

func isNotification(line string) bool {
	for _, p := range []string{"%session-changed", "%sessions-changed", "%window-add",
		"%window-close", "%window-renamed", "%layout-change", "%unlinked-window-",
		"%client-session-changed", "%client-detached", "%pane-mode-changed", "%subscription-changed"} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}
