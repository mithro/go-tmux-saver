package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

// ErrNoServer is returned by openTransport when Dial fails because no tmux
// server is listening on the configured socket (as opposed to some other
// dial failure, e.g. a bad session name against a live server).
var ErrNoServer = errors.New("no tmux server running")

func openTransport(ctx context.Context, cfg config.Config) (tmuxctl.Transport, error) {
	c, err := tmuxctl.Dial(ctx, cfg.Socket, cfg.SeedSession)
	if err != nil {
		// Only Dial's has-session preflight ("no server running" / "error
		// connecting") means no server is listening at all. Any other Dial
		// failure — e.g. "control connection closed" from an attach that
		// failed for another reason, such as a missing seed_session against
		// a live server — is a genuine misconfiguration, not a "nothing to
		// save yet" skip, and must not be swallowed into ErrNoServer.
		if strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "error connecting") {
			return nil, ErrNoServer
		}
		return nil, err
	}
	return c, nil
}

func isNoServer(err error) bool { return errors.Is(err, ErrNoServer) }

// countClients returns the number of attached tmux clients, excluding the
// control-mode client this process itself opened via Dial.
func countClients(ctx context.Context, t tmuxctl.Transport) int {
	lines, err := t.Run(ctx, "list-clients -F \"#{client_name}\"")
	if err != nil {
		return -1
	}
	n := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n - 1 // exclude ourselves (the control client)
}
