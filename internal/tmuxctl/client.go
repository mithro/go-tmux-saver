package tmuxctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

// Client is a live control-mode connection to one tmux server.
type Client struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stderr   *bytes.Buffer
	replies  chan Reply
	parseErr chan error
	mu       sync.Mutex
	desynced atomic.Bool
}

var _ Transport = (*Client)(nil)

// ErrDesynced is returned by Run once a previous command's context was
// cancelled/expired before its reply arrived. That reply may still be in
// flight on the control connection; reading further commands' replies
// without it would misattribute it to the wrong command and shift every
// later reply by one. Once desynchronised, a Client cannot be recovered —
// callers must Dial a new one.
var ErrDesynced = errors.New("control connection desynchronised after a cancelled command; dial again")

// Dial starts `tmux -L socket -C attach-session -f no-output -t session` and
// consumes the initial attach block. `-f no-output` stops pane output
// notifications flooding the connection.
//
// tmux's own stderr is captured and, if the initial attach block cannot be
// read, folded into the returned error so callers can see why the connection
// failed. In control mode tmux transparently starts a throwaway server
// rather than reporting "no server running" the way a plain client would, so
// Dial first probes with a non-control `has-session` (which never starts a
// server) to detect and report that case cleanly, without ever spawning the
// real control-mode client.
func Dial(ctx context.Context, socket, session string) (*Client, error) {
	if msg, ok := noServerRunning(ctx, socket, session); ok {
		return nil, fmt.Errorf("no server running on socket %s: %s", socket, msg)
	}
	cmd := exec.Command("tmux", "-L", socket, "-C", "attach-session", "-f", "no-output", "-t", session)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start tmux control client: %w", err)
	}
	c := &Client{cmd: cmd, stdin: stdin, stderr: &stderr, replies: make(chan Reply, 64), parseErr: make(chan error, 1)}
	go func() {
		c.parseErr <- ParseReplies(stdout, c.replies)
		close(c.replies)
	}()
	// initial attach block
	if _, err := c.next(ctx); err != nil {
		c.Close()
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("attach to %s on socket %s: %w: %s", session, socket, err, msg)
		}
		return nil, fmt.Errorf("attach to %s on socket %s: %w", session, socket, err)
	}
	return c, nil
}

// noServerRunning reports whether socket has no tmux server listening at
// all, using `has-session` (a plain, non-control client that never starts a
// server as a side effect, unlike `-C attach-session`). msg carries tmux's
// own diagnostic text for the failure. It returns ok=false both when a
// server answered and when has-session failed for some other reason (e.g.
// the server exists but session doesn't) — that case is left to the normal
// attach-session flow below.
func noServerRunning(ctx context.Context, socket, session string) (msg string, ok bool) {
	out, err := exec.CommandContext(ctx, "tmux", "-L", socket, "has-session", "-t", session).CombinedOutput()
	if err == nil {
		return "", false
	}
	m := strings.TrimSpace(string(out))
	if strings.Contains(m, "error connecting") || strings.Contains(m, "no server running") {
		return m, true
	}
	return "", false
}

func (c *Client) next(ctx context.Context) (Reply, error) {
	select {
	case r, ok := <-c.replies:
		if !ok {
			return Reply{}, fmt.Errorf("control connection closed")
		}
		return r, nil
	case <-ctx.Done():
		// The reply to whatever we just sent (or the initial attach, from
		// Dial) may still be in flight. There is no way to know which
		// future reply it is, so the connection is now unusable for
		// further correlated request/reply calls.
		c.desynced.Store(true)
		return Reply{}, ctx.Err()
	}
}

// Run sends one command and returns its reply lines. Commands are
// serialised. If an earlier Run's (or Dial's initial attach) context was
// cancelled before tmux replied, the connection is left desynchronised —
// that stray reply may still land on the wire and would otherwise be
// misread as the answer to a later command. Once that happens, Run always
// fails fast with an error wrapping ErrDesynced, without writing anything
// or attempting to drain/re-sequence replies; the caller must Dial again.
func (c *Client) Run(ctx context.Context, cmd string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.desynced.Load() {
		return nil, fmt.Errorf("run %q: %w", cmd, ErrDesynced)
	}
	if _, err := io.WriteString(c.stdin, cmd+"\n"); err != nil {
		return nil, fmt.Errorf("write %q: %w", cmd, err)
	}
	r, err := c.next(ctx)
	if err != nil {
		return nil, err
	}
	if r.Err {
		return nil, &CmdError{Cmd: cmd, Lines: r.Lines}
	}
	return r.Lines, nil
}

// Close detaches (stdin EOF → %exit) and waits for the client to exit.
func (c *Client) Close() error {
	c.stdin.Close()
	return c.cmd.Wait()
}
