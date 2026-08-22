package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

// ErrNoServer is returned by openTransport when Dial fails because no tmux
// server is listening on the configured socket (as opposed to some other
// dial failure, e.g. a bad session name against a live server).
var ErrNoServer = errors.New("no tmux server running")

// commonSetup loads the config file at cfgPath, applies the --socket/
// --data-dir flag overrides (when non-empty), validates it, and builds a
// ready (EnsureDir'd) snapshot.Store — the bring-up both "save" and
// "restore" need before they can do anything else. On failure it returns the
// message to print to stderr and the exit code to use, so both subcommands
// report config/store failures identically (exit 2 for a bad/invalid config
// or an unregistered codec, exit 1 if the store's directory can't be
// prepared on disk).
func commonSetup(cfgPath, socket, dataDir string) (cfg config.Config, store *snapshot.Store, errMsg string, exitCode int) {
	cfg, err := config.Load(cfgPath)
	if err == nil {
		if socket != "" {
			cfg.Socket = socket
		}
		if dataDir != "" {
			cfg.DataDir = dataDir
		}
		err = cfg.Validate()
	}
	if err != nil {
		return cfg, nil, "config: " + err.Error(), 2
	}

	codec, ok := snapshot.LookupCodec(cfg.Contents.Codec)
	if !ok {
		return cfg, nil, fmt.Sprintf("config: unknown codec %q", cfg.Contents.Codec), 2
	}
	st := &snapshot.Store{Dir: cfg.DataDir, Codec: codec}
	if err := st.EnsureDir(); err != nil {
		return cfg, nil, err.Error(), 1
	}
	return cfg, st, "", 0
}

// isMissingSeedSession reports whether err is the attach-time error Dial
// surfaces when the configured seed session doesn't exist on an otherwise
// live server (tmux's own "can't find session: <name>" %error text) — as
// opposed to no server listening at all (ErrNoServer) or some other genuine
// failure.
func isMissingSeedSession(err error) bool {
	return err != nil && strings.Contains(err.Error(), "can't find session")
}

func openTransport(ctx context.Context, cfg config.Config) (tmuxctl.Transport, error) {
	c, err := tmuxctl.Dial(ctx, cfg.Socket, cfg.SeedSession)
	if err != nil {
		// Only Dial's has-session preflight ("no server running" / "error
		// connecting") means no server is listening at all. Any other Dial
		// failure is a genuine misconfiguration, not a "nothing to save yet"
		// skip, and must not be swallowed into ErrNoServer — e.g. a missing
		// seed_session against a live server: tmux answers the initial
		// attach with a well-formed %error block ("can't find session: X"),
		// which Dial now surfaces directly as its own error text (it no
		// longer hands back a *Client wrapping an already-exited tmux, so
		// this doesn't masquerade as a later "control connection closed"
		// failure inside Collect).
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
