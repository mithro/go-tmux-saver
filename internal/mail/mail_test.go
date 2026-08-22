package mail

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
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

// TestRateLimiterShouldSendConcurrentExactlyOneWinner pins the atomic-create
// fix (controller ruling R34, finding 1): N goroutines racing ShouldSend for
// the same key must see exactly one true, proving the marker create is a
// single atomic O_CREATE|O_EXCL rather than a stat-then-write race where
// multiple concurrent OnFailure= invocations could all observe "no marker"
// and all decide to send.
func TestRateLimiterShouldSendConcurrentExactlyOneWinner(t *testing.T) {
	rl := RateLimiter{Dir: t.TempDir()}
	const n = 20
	var wg sync.WaitGroup
	results := make([]bool, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = rl.ShouldSend("go-tmux-saver.service", time.Now())
		}(i)
	}
	close(start)
	wg.Wait()

	trueCount := 0
	for _, r := range results {
		if r {
			trueCount++
		}
	}
	if trueCount != 1 {
		t.Fatalf("true count across %d concurrent ShouldSend calls = %d, want exactly 1", n, trueCount)
	}
}

// headerLines returns the header block of msg (everything before the first
// blank line) as individual lines, for asserting no extra header line (e.g.
// an injected "Bcc:") was created.
func headerLines(msg string) []string {
	head, _, _ := strings.Cut(msg, "\n\n")
	return strings.Split(head, "\n")
}

func TestSendSanitizesHeaderInjection(t *testing.T) {
	c := &capture{}
	evilSubject := "unit x\r\nBcc: evil@example.com"
	if err := Send(c.send, "tim@example.com", evilSubject, "the body"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msg := string(c.body)

	subjectLines := 0
	bccLines := 0
	for _, line := range headerLines(msg) {
		if strings.HasPrefix(line, "Subject:") {
			subjectLines++
		}
		if strings.HasPrefix(line, "Bcc:") {
			bccLines++
		}
	}
	if subjectLines != 1 {
		t.Fatalf("message =\n%q\nwant exactly one Subject: line, got %d", msg, subjectLines)
	}
	if bccLines != 0 {
		t.Fatalf("message =\n%q\nwant no Bcc: header line (CR/LF injection not sanitized)", msg)
	}
	if strings.Contains(msg, "\r") {
		t.Fatalf("message =\n%q\nwant no raw CR left over from the injected subject", msg)
	}
	// Exactly one blank line still separates headers from the body, and the
	// injected text is neutered into the Subject value rather than becoming
	// its own header line.
	want := "To: tim@example.com\n" +
		"Subject: unit x Bcc: evil@example.com\n" +
		"Content-Type: text/plain; charset=utf-8\n" +
		"\n" +
		"the body"
	if msg != want {
		t.Fatalf("message =\n%q\nwant\n%q", msg, want)
	}
}

func TestSendSanitizesToHeaderInjection(t *testing.T) {
	c := &capture{}
	if err := Send(c.send, "tim@example.com\r\nBcc: evil@example.com", "subject", "body"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msg := string(c.body)
	for _, line := range headerLines(msg) {
		if strings.HasPrefix(line, "Bcc:") {
			t.Fatalf("message =\n%q\nwant no Bcc: header line injected via To:", msg)
		}
	}
}

func TestSubjectFormat(t *testing.T) {
	if got, want := Subject("host", "go-tmux-saver.service", false), "[go-tmux-saver] host: go-tmux-saver.service failed"; got != want {
		t.Fatalf("Subject(recovered=false) = %q, want %q", got, want)
	}
	if got, want := Subject("host", "go-tmux-saver.service", true), "[go-tmux-saver] host: go-tmux-saver.service recovered"; got != want {
		t.Fatalf("Subject(recovered=true) = %q, want %q", got, want)
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
