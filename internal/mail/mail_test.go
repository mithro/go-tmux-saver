package mail

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// capture is a fake sendmail func: it records the message bytes it was
// handed and returns errToReturn (nil by default).
type capture struct {
	body        []byte
	errToReturn error
}

func (c *capture) send(body []byte) error {
	c.body = body
	return c.errToReturn
}

func TestSendBuildsRFC822Message(t *testing.T) {
	c := &capture{}
	if err := Send(c.send, "tim@example.com", "[go-tmux-saver] host: unit failed", "line one\nline two\n"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	want := "To: tim@example.com\n" +
		"Subject: [go-tmux-saver] host: unit failed\n" +
		"Content-Type: text/plain; charset=utf-8\n" +
		"\n" +
		"line one\nline two\n"
	if got := string(c.body); got != want {
		t.Fatalf("body =\n%q\nwant\n%q", got, want)
	}
}

func TestSendPropagatesSendmailError(t *testing.T) {
	wantErr := errors.New("boom")
	c := &capture{errToReturn: wantErr}
	err := Send(c.send, "tim@example.com", "subject", "body")
	if !errors.Is(err, wantErr) {
		t.Fatalf("Send err = %v, want %v", err, wantErr)
	}
}

func TestRateLimiterShouldSendOnceThenSuppressed(t *testing.T) {
	rl := RateLimiter{Dir: t.TempDir()}
	now := time.Now()
	if !rl.ShouldSend("go-tmux-saver.service", now) {
		t.Fatal("first ShouldSend = false, want true (first failure of the streak)")
	}
	if rl.ShouldSend("go-tmux-saver.service", now.Add(time.Minute)) {
		t.Fatal("second ShouldSend = true, want false (marker still present)")
	}
	if rl.ShouldSend("go-tmux-saver.service", now.Add(2*time.Minute)) {
		t.Fatal("third ShouldSend = true, want false (marker still present)")
	}
}

func TestRateLimiterClearThenShouldSendAgain(t *testing.T) {
	rl := RateLimiter{Dir: t.TempDir()}
	now := time.Now()
	if !rl.ShouldSend("go-tmux-saver.service", now) {
		t.Fatal("first ShouldSend = false, want true")
	}
	if !rl.Clear("go-tmux-saver.service") {
		t.Fatal("Clear = false, want true (marker existed)")
	}
	if rl.Clear("go-tmux-saver.service") {
		t.Fatal("second Clear = true, want false (marker already gone)")
	}
	if !rl.ShouldSend("go-tmux-saver.service", now.Add(time.Hour)) {
		t.Fatal("ShouldSend after Clear = false, want true (new streak)")
	}
}

func TestRateLimiterSanitizesKeyForFilename(t *testing.T) {
	dir := t.TempDir()
	rl := RateLimiter{Dir: dir}
	if !rl.ShouldSend("go-tmux-saver-watch/foo bar.service", time.Now()) {
		t.Fatal("ShouldSend = false, want true")
	}
	// The marker must land directly inside dir (sanitized, no subdirs
	// created from the "/" in the key).
	matches, err := filepath.Glob(filepath.Join(dir, "alert-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("markers in dir = %v, want exactly 1", matches)
	}
}
