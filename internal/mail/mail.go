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

// sanitizeHeader makes s safe for use as (the whole of) a single RFC-822
// header field body: CR and LF are replaced with a single space, so a value
// coming from an untrusted source (e.g. --unit) can't inject additional
// header lines (a bare "\r\n" would otherwise terminate the header and let
// attacker-controlled text add e.g. a "Bcc:" line) or terminate the header
// block early.
func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// Subject builds the standard go-tmux-saver alert subject line:
// "[go-tmux-saver] <host>: <unit> failed" or "... recovered" when recovered
// is true. Shared by the alert subcommand and save --auto's recovery hook so
// both produce an identical format.
func Subject(host, unit string, recovered bool) string {
	verb := "failed"
	if recovered {
		verb = "recovered"
	}
	return fmt.Sprintf("[go-tmux-saver] %s: %s %s", host, unit, verb)
}

// Send builds an RFC-822 message (To:, Subject:, and a
// "Content-Type: text/plain; charset=utf-8" header, a blank line, then body)
// and hands the resulting bytes to sendmail. to and subject are sanitized
// against header injection (embedded CR/LF) before being written.
func Send(sendmail func(body []byte) error, to, subject, body string) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "To: %s\n", sanitizeHeader(to))
	fmt.Fprintf(&buf, "Subject: %s\n", sanitizeHeader(subject))
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
//
// The marker is created with O_CREATE|O_EXCL so two concurrent callers for
// the same key (e.g. overlapping OnFailure= invocations) can't both observe
// "no marker" and both decide to send: exactly one O_EXCL create wins, the
// other gets EEXIST and returns false.
func (r RateLimiter) ShouldSend(key string, now time.Time) bool {
	p := r.markerPath(key)
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		// Can't persist rate-limit state; fail open so the alert still goes
		// out rather than silently vanishing.
		return true
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// Marker already armed — either by an earlier ShouldSend in this
			// streak or a concurrent racer that won the create; either way,
			// this call must not send.
			return false
		}
		// Some other failure (e.g. permission denied): can't persist
		// rate-limit state, so fail open rather than silently dropping the
		// alert.
		return true
	}
	defer f.Close()
	_, _ = f.WriteString(now.UTC().Format(time.RFC3339) + "\n")
	return true
}

// Clear removes key's marker (ending its failure streak) and reports whether
// one was present — the caller uses that to decide whether to send exactly
// one recovery mail.
func (r RateLimiter) Clear(key string) bool {
	err := os.Remove(r.markerPath(key))
	return err == nil
}
