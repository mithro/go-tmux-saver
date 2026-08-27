package tmuxctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientRoundTrip(t *testing.T) {
	sock := StartTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, sock, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	lines, err := c.Run(ctx, `list-windows -t default -F "#{window_index}\t#{window_name}"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "0\th" {
		t.Fatalf("list-windows = %q", lines)
	}
	if _, err := c.Run(ctx, "list-windows -t nosuchsession"); err == nil {
		t.Fatal("expected error for bad target")
	} else if !strings.Contains(err.Error(), "nosuchsession") {
		t.Fatalf("error should carry tmux message, got %v", err)
	}
	// second command after an error still works on the same connection
	if _, err := c.Run(ctx, "list-sessions"); err != nil {
		t.Fatal(err)
	}
}

func TestClientContextTimeout(t *testing.T) {
	sock := StartTestServer(t)
	c, err := Dial(context.Background(), sock, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if _, err := c.Run(ctx, "list-sessions"); err == nil {
		t.Fatal("expected context deadline error")
	}
}

// nopWriteCloser lets a hand-built Client accept Run's command write.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestClientDesyncAfterCancel(t *testing.T) {
	// White-box on purpose: against a real server the 1ns-deadline version
	// raced tmux's (fast) reply against ctx.Done() in next()'s select —
	// when both were ready, select's random choice made the test flake
	// (seen under -race). Here the reply channel simply never delivers, so
	// the cancelled context is the only ready case, deterministically.
	var sink bytes.Buffer
	c := &Client{stdin: nopWriteCloser{&sink}, replies: make(chan Reply), parseErr: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Run(ctx, "list-sessions"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context error, got %v", err)
	}
	// The stray reply to the cancelled command may still be in flight; a
	// later Run, even with a fresh context, must not read it as its own
	// answer — it must fail fast with ErrDesynced instead.
	if _, err := c.Run(context.Background(), "list-sessions"); !errors.Is(err, ErrDesynced) {
		t.Fatalf("expected ErrDesynced after a cancelled command, got %v", err)
	}
}

func TestDialNoServer(t *testing.T) {
	sock := fmt.Sprintf("gts-nonexistent-%d", os.Getpid())
	c, err := Dial(context.Background(), sock, "default")
	if err == nil {
		c.Close()
		t.Fatal("expected error dialing nonexistent server")
	}
	if !strings.Contains(err.Error(), "no server running") {
		t.Fatalf("error should carry tmux stderr message, got %v", err)
	}
}

// TestDialBadSession covers a live server that simply doesn't have the
// requested session: tmux answers the initial attach with a well-formed
// %begin...%error...%end/%exit block rather than failing to reply at all, so
// this is a distinct failure mode from TestDialNoServer (no server) — Dial
// must notice reply.Err on the initial attach and fail instead of handing
// back a *Client wrapping an already-exited tmux.
func TestDialBadSession(t *testing.T) {
	sock := StartTestServer(t)
	c, err := Dial(context.Background(), sock, "nonexistent-session")
	if err == nil {
		c.Close()
		t.Fatal("expected error dialing a nonexistent session against a live server")
	}
	if !strings.Contains(err.Error(), "can't find session") && !strings.Contains(err.Error(), "nonexistent-session") {
		t.Fatalf("error should carry tmux's %%error text, got %v", err)
	}
	// Dial must have closed the failed attach's control client rather than
	// leaking it — no client should be left attached on the socket.
	out, lerr := exec.Command("tmux", "-L", sock, "list-clients").CombinedOutput()
	if lerr == nil && len(strings.TrimSpace(string(out))) != 0 {
		t.Fatalf("expected no leaked tmux clients on %s, got %q", sock, out)
	}
}

// TestNoServerClassifiesBySocketState covers issue #6: "no server" must be
// decided from the socket itself (missing file, connection-refused stale
// socket) rather than by matching tmux's English error text, which can
// change between versions/locales. All three states run under a private
// TMUX_TMPDIR so nothing touches the user's real socket directory.
func TestNoServerClassifiesBySocketState(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", t.TempDir())

	// 1. No socket file at all → no server.
	if msg, ok := noServerRunning(context.Background(), "gts-no-such-socket", "default"); !ok {
		t.Errorf("missing socket: ok=false msg=%q, want no-server", msg)
	}

	// 2. A stale socket file (nothing listening) → no server.
	dir := filepath.Join(os.Getenv("TMUX_TMPDIR"), fmt.Sprintf("tmux-%d", os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "gts-stale")
	l, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("stale socket file should remain: %v", err)
	}
	if msg, ok := noServerRunning(context.Background(), "gts-stale", "default"); !ok {
		t.Errorf("stale socket: ok=false msg=%q, want no-server", msg)
	}

	// 3. A live server → NOT no-server (even though the session may or may
	// not exist — that classification belongs to the attach flow).
	// Started by hand rather than via StartTestServer: its test-name-derived
	// socket name plus the TMUX_TMPDIR above would overflow the ~108-byte
	// unix socket path limit.
	sock := "gts-live"
	if out, err := exec.Command("tmux", "-L", sock, "-f", "/dev/null", "new-session", "-d", "-s", "default", "-n", "h", "tail -f /dev/null").CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("tmux", "-L", sock, "kill-server").Run() })
	if msg, ok := noServerRunning(context.Background(), sock, "default"); ok {
		t.Errorf("live server: ok=true msg=%q, want server-present", msg)
	}
	if msg, ok := noServerRunning(context.Background(), sock, "no-such-session"); ok {
		t.Errorf("live server, missing session: ok=true msg=%q, want server-present", msg)
	}
}

// TestNextSurfacesParseErrDetail covers issue #7's diagnostics half: when
// the control stream dies inside an open %begin block, the error must carry
// ParseReplies' detail ("ended inside block N"), not just a generic
// "control connection closed" — and repeated calls must keep returning it
// without blocking (the one-shot parseErr channel is captured once).
func TestNextSurfacesParseErrDetail(t *testing.T) {
	c := &Client{replies: make(chan Reply, 4), parseErr: make(chan error, 1)}
	r := strings.NewReader("%begin 1 7 0\npartial output\n") // EOF mid-block
	go func() {
		c.parseErr <- ParseReplies(r, c.replies)
		close(c.replies)
	}()
	for i := 0; i < 2; i++ {
		_, err := c.next(context.Background())
		if err == nil || !strings.Contains(err.Error(), "ended inside block 7") {
			t.Fatalf("call %d: err = %v, want the ended-inside-block detail", i, err)
		}
	}

	// A clean close (%exit) keeps the plain message, with no wrapped nil.
	c2 := &Client{replies: make(chan Reply, 4), parseErr: make(chan error, 1)}
	go func() {
		c2.parseErr <- ParseReplies(strings.NewReader("%exit\n"), c2.replies)
		close(c2.replies)
	}()
	_, err := c2.next(context.Background())
	if err == nil || err.Error() != "control connection closed" {
		t.Fatalf("clean close err = %v, want plain control-connection-closed", err)
	}
}
