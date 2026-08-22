package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
