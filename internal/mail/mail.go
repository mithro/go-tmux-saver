// Package mail sends go-tmux-saver's failure/recovery alerts via the local
// sendmail(1) binary, with a per-key rate limiter so a wedged unit doesn't
// spam an inbox on every timer tick.
package mail

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// Sendmail is the real (non-test) sender: it execs `sendmail -t` with the
// message body on stdin, capturing stderr into the returned error on
// failure. It is a package var — the sole injectable seam — so CLI tests can
// swap in a fake and never invoke a real sendmail binary.
var Sendmail = func(body []byte) error {
	cmd := exec.Command("sendmail", "-t")
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sendmail: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Send builds an RFC-822 message (To:, Subject:, and a
// "Content-Type: text/plain; charset=utf-8" header, a blank line, then body)
// and hands the resulting bytes to sendmail.
func Send(sendmail func(body []byte) error, to, subject, body string) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "To: %s\n", to)
	fmt.Fprintf(&buf, "Subject: %s\n", subject)
	fmt.Fprintf(&buf, "Content-Type: text/plain; charset=utf-8\n")
	fmt.Fprintf(&buf, "\n")
	buf.WriteString(body)
	return sendmail(buf.Bytes())
}

// RateLimiter suppresses repeat alerts for the same key (typically a systemd
// unit name) by dropping a marker file under Dir on the first failure of a
// streak, and refusing to send again until Clear removes it.
type RateLimiter struct {
	Dir string
}

// sanitizeKey makes key safe for use as (part of) a filename: "/" and any
// whitespace are replaced with "_".
func sanitizeKey(key string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || unicode.IsSpace(r) {
			return '_'
		}
		return r
	}, key)
}

func (r RateLimiter) markerPath(key string) string {
	return filepath.Join(r.Dir, "alert-"+sanitizeKey(key))
}

// ShouldSend reports whether an alert for key should be sent right now: true
// on the first failure of a streak (it creates the marker), false while the
// marker from an earlier ShouldSend call is still present.
func (r RateLimiter) ShouldSend(key string, now time.Time) bool {
	p := r.markerPath(key)
	if _, err := os.Stat(p); err == nil {
		return false
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		// Can't persist rate-limit state; fail open so the alert still goes
		// out rather than silently vanishing.
		return true
	}
	_ = os.WriteFile(p, []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o600)
	return true
}

// Clear removes key's marker (ending its failure streak) and reports whether
// one was present — the caller uses that to decide whether to send exactly
// one recovery mail.
func (r RateLimiter) Clear(key string) bool {
	err := os.Remove(r.markerPath(key))
	return err == nil
}
