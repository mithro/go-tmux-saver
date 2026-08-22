package tmuxctl

import (
	"context"
	"fmt"
	"os"
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
