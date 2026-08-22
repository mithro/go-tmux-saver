package tmuxctl

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func collect(t *testing.T, input string) []Reply {
	t.Helper()
	ch := make(chan Reply, 16)
	if err := ParseReplies(strings.NewReader(input), ch); err != nil {
		t.Fatal(err)
	}
	close(ch)
	var out []Reply
	for r := range ch {
		out = append(out, r)
	}
	return out
}

func TestParseProbeTranscript(t *testing.T) {
	data, err := os.ReadFile("testdata/probe.transcript")
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, string(data))
	if len(got) != 4 {
		t.Fatalf("want 4 replies, got %d: %+v", len(got), got)
	}
	if len(got[0].Lines) != 0 { // attach block contains only a %session-changed notification
		t.Errorf("attach reply should have no data lines, got %q", got[0].Lines)
	}
	if got[1].Lines[0] != "probe" {
		t.Errorf("list-sessions reply = %q", got[1].Lines)
	}
	if !strings.HasPrefix(got[2].Lines[0], "probe\t0\t0\t%0\t") {
		t.Errorf("list-panes reply = %q", got[2].Lines)
	}
	if len(got[3].Lines) != 3 || !strings.Contains(got[3].Lines[0], "tim@ten64") {
		t.Errorf("capture-pane reply = %q", got[3].Lines)
	}
}

func TestParseErrorBlock(t *testing.T) {
	got := collect(t, "%begin 1 2 1\nno such session: x\n%error 1 2 1\n%exit\n")
	if len(got) != 1 || !got[0].Err || got[0].Lines[0] != "no such session: x" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseMismatchedEndIsData(t *testing.T) {
	// an %end whose number does not match the open %begin is pane data, not a terminator
	got := collect(t, "%begin 1 5 1\n%end 1 999 1\nreal\n%end 1 5 1\n")
	if len(got) != 1 || len(got[0].Lines) != 2 || got[0].Lines[0] != "%end 1 999 1" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseNotificationWhitelist(t *testing.T) {
	// Notifications matching the whitelist should be filtered.
	// Pane data like "%outputs are fine" should NOT be filtered (prefix but not exact match).
	input := "%begin 1 10 1\n%window-renamed @1 foo\n%output %0 abc\n%outputs are fine\n%end 1 10 1\n%exit\n"
	got := collect(t, input)
	if len(got) != 1 {
		t.Fatalf("want 1 reply, got %d", len(got))
	}
	if len(got[0].Lines) != 1 || got[0].Lines[0] != "%outputs are fine" {
		t.Errorf("want [\"%s\"], got %q", "%outputs are fine", got[0].Lines)
	}
}

func TestParseEOFInsideBlock(t *testing.T) {
	// EOF while a block is open should return an error wrapping io.ErrUnexpectedEOF.
	ch := make(chan Reply, 16)
	err := ParseReplies(strings.NewReader("%begin 1 7 1\npartial\n"), ch)
	close(ch)

	// Verify error wraps io.ErrUnexpectedEOF.
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("want error wrapping io.ErrUnexpectedEOF, got %v", err)
	}

	// Verify nothing was sent on the channel.
	if len(ch) > 0 {
		t.Errorf("want no replies sent, got %d", len(ch))
	}
}
