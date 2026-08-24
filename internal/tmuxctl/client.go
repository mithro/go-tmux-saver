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
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/mithro/go-tmux-saver/internal/trace"
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
	// parseOnce/finalParseErr capture the read loop's one-shot exit error
	// the first time the closed replies channel is observed, so every later
	// next() call can keep reporting it without blocking on the
	// already-drained parseErr channel (issue #7).
	parseOnce     sync.Once
	finalParseErr error
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
	stop := trace.Time("dial.has-session")
	msg, noServer := noServerRunning(ctx, socket, session)
	stop()
	if noServer {
		return nil, fmt.Errorf("no server running on socket %s: %s", socket, msg)
	}
	defer trace.Time("dial.attach")()
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
	reply, err := c.next(ctx)
	if err != nil {
		c.Close()
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("attach to %s on socket %s: %w: %s", session, socket, err, msg)
		}
		return nil, fmt.Errorf("attach to %s on socket %s: %w", session, socket, err)
	}
	if reply.Err {
		// tmux answered the initial attach with a well-formed %error block
		// (e.g. "can't find session: X") rather than failing to reply at
		// all — the control client itself started fine, so the earlier
		// os.exec-level failure paths above don't catch this. Close the
		// now-useless client (it immediately %exits after an attach error)
		// so it isn't leaked, and surface tmux's own error text.
		c.Close()
		return nil, fmt.Errorf("attach to %s on socket %s: %s", session, socket, strings.Join(reply.Lines, " "))
	}
	return c, nil
}

// socketPath returns the filesystem path of the socket tmux -L <name> uses:
// $TMUX_TMPDIR (or /tmp) / tmux-<uid> / <name>.
func socketPath(name string) string {
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	return filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), name)
}

// noServerRunning reports whether socket has no tmux server listening at
// all. msg carries the diagnostic text for the failure. It returns ok=false
// both when a server answered and when the probe failed for some other
// reason (e.g. the server exists but session doesn't) — that case is left
// to the normal attach-session flow below.
//
// Issue #6: the primary signal is the socket itself — a missing socket
// file, or a connect refused on a stale one — because that classification
// is errno-based and survives tmux rewording (or localising) its error
// messages. Only when the socket state is unclassifiable (e.g. an EACCES
// stat, some other connect errno) does it fall back to running
// `has-session` (a plain, non-control client that never starts a server as
// a side effect, unlike `-C attach-session`) and matching tmux's text.
func noServerRunning(ctx context.Context, socket, session string) (msg string, ok bool) {
	path := socketPath(socket)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Sprintf("socket %s does not exist", path), true
		}
		// Unclassifiable stat failure — fall through to the text probe.
	} else {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", path)
		if err == nil {
			conn.Close()
			return "", false // a server is listening
		}
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			return fmt.Sprintf("stale socket %s: %v", path, err), true
		}
		// Unclassifiable connect failure — fall through to the text probe.
	}
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
			// The read loop has exited — surface WHY (issue #7): an EOF
			// inside an open %begin block is a very different failure from
			// a clean close, and a bare "control connection closed" hid
			// that detail. The goroutine sends its result before closing
			// replies, so this receive never blocks.
			c.parseOnce.Do(func() { c.finalParseErr = <-c.parseErr })
			if c.finalParseErr != nil {
				return Reply{}, fmt.Errorf("control connection closed: %w", c.finalParseErr)
			}
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
