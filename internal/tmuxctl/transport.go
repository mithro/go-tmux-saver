package tmuxctl

import (
	"context"
	"fmt"
	"strings"
)

// Transport runs tmux commands and returns reply lines.
type Transport interface {
	Run(ctx context.Context, cmd string) ([]string, error)
	Close() error
}

// CmdError is returned when tmux answers a command with %error.
type CmdError struct {
	Cmd   string
	Lines []string
}

func (e *CmdError) Error() string {
	return fmt.Sprintf("tmux %q: %s", e.Cmd, strings.Join(e.Lines, " "))
}

// Fake is a Transport backed by a command→reply map, for tests.
type Fake struct {
	Replies map[string][]string
	Default []string // reply for commands not in Replies (nil = error)
	Calls   []string
}

func (f *Fake) Run(_ context.Context, cmd string) ([]string, error) {
	f.Calls = append(f.Calls, cmd)
	if r, ok := f.Replies[cmd]; ok {
		return append([]string(nil), r...), nil
	}
	if f.Default != nil {
		return append([]string(nil), f.Default...), nil
	}
	return nil, &CmdError{Cmd: cmd, Lines: []string{"fake: no reply configured"}}
}

func (f *Fake) Close() error { return nil }
