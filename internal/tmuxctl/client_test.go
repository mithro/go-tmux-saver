package tmuxctl

import (
	"context"
	"errors"
	"fmt"
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

func TestClientDesyncAfterCancel(t *testing.T) {
	sock := StartTestServer(t)
	c, err := Dial(context.Background(), sock, "default")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if _, err := c.Run(ctx, "list-sessions"); err == nil {
		t.Fatal("expected context deadline error")
	}
	// The stray reply to the cancelled command may still be in flight; a
	// later Run, even with a fresh context, must not read it as its own
	// answer — it must fail fast with ErrDesynced instead.
	if _, err := c.Run(context.Background(), "list-sessions"); !errors.Is(err, ErrDesynced) {
		t.Fatalf("expected ErrDesynced after a cancelled command, got %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close after desync: %v", err)
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
