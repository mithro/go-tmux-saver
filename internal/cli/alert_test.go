package cli

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/mithro/go-tmux-saver/internal/mail"
)

var errBoom = errors.New("boom")

// fakeSendmail installs a capturing fake in place of mail.Sendmail for the
// duration of the test, restoring the real one on cleanup. It is safe for
// sequential (not concurrent) subtests since mail.Sendmail is a single
// package var.
func fakeSendmail(t *testing.T) *sentMail {
	t.Helper()
	real := mail.Sendmail
	s := &sentMail{}
	mail.Sendmail = func(body []byte) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.bodies = append(s.bodies, body)
		return s.errToReturn
	}
	t.Cleanup(func() { mail.Sendmail = real })
	return s
}

type sentMail struct {
	mu          sync.Mutex
	bodies      [][]byte
	errToReturn error
}

func (s *sentMail) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *sentMail) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		return ""
	}
	return string(s.bodies[len(s.bodies)-1])
}

// TestAlertCLIFailureSendsThenRateLimits covers the failure-alert path: the
// first `alert --unit ...` call sends (subject contains "failed" and the
// unit name), the second identical call is rate-limited (no mail, exit 0).
func TestAlertCLIFailureSendsThenRateLimits(t *testing.T) {
	s := fakeSendmail(t)
	cfgPath := writeConfig(t, `{"mail_to": "ops@example.com"}`)
	dataDir := t.TempDir()

	var out, errb bytes.Buffer
	code := Run([]string{"alert", "--unit", "go-tmux-saver.service", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if s.count() != 1 {
		t.Fatalf("sendmail calls = %d, want 1", s.count())
	}
	if !strings.Contains(s.last(), "Subject: [go-tmux-saver]") || !strings.Contains(s.last(), "go-tmux-saver.service failed") {
		t.Fatalf("message = %q, want a failed-subject mentioning the unit", s.last())
	}
	if !strings.Contains(s.last(), "To: ops@example.com") {
		t.Fatalf("message = %q, want To: ops@example.com", s.last())
	}

	out.Reset()
	errb.Reset()
	code = Run([]string{"alert", "--unit", "go-tmux-saver.service", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("second call exit %d, want 0 (rate-limited, not an error); stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if s.count() != 1 {
		t.Fatalf("sendmail calls after second (rate-limited) call = %d, want still 1", s.count())
	}
}

// TestAlertCLIRecoveredSendsOnce covers --recovered: it sends exactly once
// (Clear returns true the first time), and a second --recovered call with no
// intervening failure sends nothing.
func TestAlertCLIRecoveredSendsOnce(t *testing.T) {
	s := fakeSendmail(t)
	cfgPath := writeConfig(t, "{}")
	dataDir := t.TempDir()

	// Establish a failure streak first so Clear has something to clear.
	var out, errb bytes.Buffer
	Run([]string{"alert", "--unit", "go-tmux-saver.service", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if s.count() != 1 {
		t.Fatalf("setup: sendmail calls = %d, want 1", s.count())
	}

	out.Reset()
	errb.Reset()
	code := Run([]string{"alert", "--unit", "go-tmux-saver.service", "--recovered", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if s.count() != 2 {
		t.Fatalf("sendmail calls = %d, want 2 (failure + recovery)", s.count())
	}
	if !strings.Contains(s.last(), "go-tmux-saver.service recovered") {
		t.Fatalf("message = %q, want a recovered subject mentioning the unit", s.last())
	}

	out.Reset()
	errb.Reset()
	code = Run([]string{"alert", "--unit", "go-tmux-saver.service", "--recovered", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 0 {
		t.Fatalf("second --recovered exit %d, want 0; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if s.count() != 2 {
		t.Fatalf("sendmail calls after second --recovered (nothing to clear) = %d, want still 2", s.count())
	}
}

// TestAlertCLIRequiresUnit covers the --unit-required guard.
func TestAlertCLIRequiresUnit(t *testing.T) {
	fakeSendmail(t)
	cfgPath := writeConfig(t, "{}")
	dataDir := t.TempDir()

	var out, errb bytes.Buffer
	code := Run([]string{"alert", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit %d, want 2; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
}

// TestAlertCLISendmailFailureExitsOne pins the brief's exit-code contract:
// exit 1 only when sendmail itself fails.
func TestAlertCLISendmailFailureExitsOne(t *testing.T) {
	s := fakeSendmail(t)
	s.errToReturn = errBoom
	cfgPath := writeConfig(t, "{}")
	dataDir := t.TempDir()

	var out, errb bytes.Buffer
	code := Run([]string{"alert", "--unit", "go-tmux-saver.service", "--config", cfgPath, "--data-dir", dataDir}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
}
