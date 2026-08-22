package setup

import "io"

// Env bundles the side effects Install, Validate, and Update need, so tests
// can drive them against a temp directory and recording fakes instead of
// the real filesystem/systemd/tmux.
//
// Systemctl doubles as a generic "run a systemd tool" injectable: real
// wiring (internal/cli/setup.go) inspects the args it's called with and
// dispatches to either the `systemctl` or `systemd-analyze` binary — calls
// shaped like ("--user", "verify", <unit paths>...) run via
// `systemd-analyze --user verify ...` (systemd-analyze accepts --user too),
// everything else runs via `systemctl`. This keeps Env's shape exactly as
// specified (a single Systemctl field) rather than adding a second
// SystemdAnalyze injectable.
type Env struct {
	ConfigHome   string
	Systemctl    func(args ...string) (string, error)
	TmuxBindings func() (string, error)
	Stdout       io.Writer
}
