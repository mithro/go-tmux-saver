package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/setup"
)

// defaultConfigHome returns $XDG_CONFIG_HOME (or ~/.config), the directory
// setup.Managed.Rel paths are relative to. It's derived from config.Path()
// (base/go-tmux-saver/config.json) rather than duplicating the XDG lookup,
// so it always agrees with where the config file itself lives.
func defaultConfigHome() string {
	return filepath.Dir(filepath.Dir(config.Path()))
}

// resolveBinary returns the absolute, symlink-resolved path to the
// currently running go-tmux-saver binary, for use as setup.Params.Binary in
// the rendered ExecStart= lines.
func resolveBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve binary: %w", err)
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve binary: %w", err)
	}
	return real, nil
}

// execTimeout bounds every real process execCommand runs (systemctl,
// systemd-analyze, tmux list-keys). One shared bound keeps the seam's
// signature exactly `func(name string, args ...string) ([]byte, error)` —
// no per-call context parameter — at the cost of using the same timeout for
// both the interactive `setup` subcommands and status's quick timer check.
const execTimeout = 15 * time.Second

// execCommand is the seam every real subprocess call in this file goes
// through: run name(args...) to completion and return its combined
// stdout+stderr. Tests swap this package var for a fake to assert dispatch
// (systemctl vs. systemd-analyze, tmux list-keys, `systemctl --user
// is-active` for timerState) without touching the real system.
var execCommand = func(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// realSystemctl is Env.Systemctl's real (non-test) implementation. It
// doubles as a generic "run a systemd tool" injectable (see Env's doc
// comment in internal/setup/env.go): a call shaped like
// ("--user", "verify", <unit paths>...) runs via `systemd-analyze` (which
// also accepts --user) instead of `systemctl`, since "verify" isn't a
// systemctl verb.
func realSystemctl(args ...string) (string, error) {
	bin := "systemctl"
	if len(args) >= 2 && args[1] == "verify" {
		bin = "systemd-analyze"
	}
	out, err := execCommand(bin, args...)
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	return string(out), nil
}

// realTmuxBindings returns an Env.TmuxBindings implementation that lists
// key bindings from cfg's tmux socket.
func realTmuxBindings(cfg config.Config) func() (string, error) {
	return func() (string, error) {
		out, err := execCommand("tmux", "-L", cfg.Socket, "list-keys")
		if err != nil {
			return string(out), fmt.Errorf("tmux -L %s list-keys: %w", cfg.Socket, err)
		}
		return string(out), nil
	}
}

func realSetupEnv(cfg config.Config, stdout io.Writer) setup.Env {
	return setup.Env{
		ConfigHome:   defaultConfigHome(),
		Systemctl:    realSystemctl,
		TmuxBindings: realTmuxBindings(cfg),
		Stdout:       stdout,
	}
}

// loadSetupCfg loads and validates the config file at cfgPath, the same way
// commonSetup does, but without commonSetup's snapshot.Store bring-up
// (setup's subcommands don't need the data dir — they only need cfg's
// values to render templates and to point TmuxBindings at the right
// socket).
func loadSetupCfg(cfgPath string) (config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func setupParams(cfg config.Config) (setup.Params, error) {
	bin, err := resolveBinary()
	if err != nil {
		return setup.Params{}, err
	}
	return setup.Params{
		Version:         Version,
		Binary:          bin,
		Socket:          cfg.Socket,
		SeedSession:     cfg.SeedSession,
		SeedWindow:      cfg.SeedWindow,
		IntervalMinutes: cfg.IntervalMinutes,
		MailTo:          cfg.MailTo,
	}, nil
}

// renderSetupFiles loads cfg from cfgPath and renders the managed files, the
// common bring-up every `setup` subcommand needs.
func renderSetupFiles(cfgPath string, stderr io.Writer) (config.Config, []setup.Managed, int) {
	cfg, err := loadSetupCfg(cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "config:", err)
		return cfg, nil, 2
	}
	p, err := setupParams(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "setup:", err)
		return cfg, nil, 1
	}
	files, err := setup.Render(p)
	if err != nil {
		fmt.Fprintln(stderr, "setup: render:", err)
		return cfg, nil, 1
	}
	return cfg, files, 0
}

func runSetupGenerate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup generate", flag.ContinueOnError)
	dir := fs.String("dir", defaultConfigHome(), "directory to render managed files into (Rel-relative), without touching systemd/tmux")
	cfgPath := fs.String("config", config.Path(), "config file")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	_, files, code := renderSetupFiles(*cfgPath, stderr)
	if code != 0 {
		return code
	}
	if err := setup.WriteFiles(*dir, files); err != nil {
		fmt.Fprintln(stderr, "setup generate:", err)
		return 1
	}
	for _, f := range files {
		fmt.Fprintln(stdout, filepath.Join(*dir, f.Rel))
	}
	return 0
}

func runSetupInstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup install", flag.ContinueOnError)
	cfgPath := fs.String("config", config.Path(), "config file")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, files, code := renderSetupFiles(*cfgPath, stderr)
	if code != 0 {
		return code
	}
	env := realSetupEnv(cfg, stdout)
	if err := setup.Install(env, files); err != nil {
		fmt.Fprintln(stderr, "setup install:", err)
		return 1
	}
	fmt.Fprintf(stdout, "installed %d files under %s\n", len(files), env.ConfigHome)
	return 0
}

func runSetupValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup validate", flag.ContinueOnError)
	cfgPath := fs.String("config", config.Path(), "config file")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, files, code := renderSetupFiles(*cfgPath, stderr)
	if code != 0 {
		return code
	}
	env := realSetupEnv(cfg, stdout)
	drifts := setup.Validate(env, files)
	if len(drifts) == 0 {
		fmt.Fprintln(stdout, "ok: no drift")
		return 0
	}
	for _, d := range drifts {
		fmt.Fprintf(stdout, "%s: %s\n", d.Kind, d.Path)
		if d.Diff != "" {
			fmt.Fprintln(stdout, d.Diff)
		}
	}
	return 1
}

func runSetupUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup update", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "show what would change without writing or restarting anything")
	cfgPath := fs.String("config", config.Path(), "config file")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, files, code := renderSetupFiles(*cfgPath, stderr)
	if code != 0 {
		return code
	}
	env := realSetupEnv(cfg, stdout)
	changed, err := setup.Update(env, files, *dryRun)
	if err != nil {
		fmt.Fprintln(stderr, "setup update:", err)
		return 1
	}
	if len(changed) == 0 {
		fmt.Fprintln(stdout, "up to date")
	}
	return 0
}

// realTimerState is status.go's timerState replacement: the live
// `systemctl --user is-active go-tmux-saver.timer` state, via execCommand
// so tests can fake it. Any exec failure (systemctl missing, no user
// systemd instance, timeout, non-zero exit for e.g. "inactive"/"failed"
// reported as an error by is-active) reports "unknown" rather than
// propagating an error status has no way to show.
func realTimerState() string {
	out, err := execCommand("systemctl", "--user", "is-active", "go-tmux-saver.timer")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func init() {
	// Task 16 replaces status.go's placeholder timerState (which always
	// reported "unknown") with the real check: the go-tmux-saver.timer's
	// live systemd --user state.
	timerState = realTimerState

	register(command{"setup", "manage go-tmux-saver's systemd --user units and tmux.conf snippet (generate|install|validate|update)", func(args []string, stdout, stderr io.Writer) int {
		if len(args) == 0 {
			fmt.Fprintln(stderr, "usage: go-tmux-saver setup generate|install|validate|update [flags]")
			return 2
		}
		switch args[0] {
		case "generate":
			return runSetupGenerate(args[1:], stdout, stderr)
		case "install":
			return runSetupInstall(args[1:], stdout, stderr)
		case "validate":
			return runSetupValidate(args[1:], stdout, stderr)
		case "update":
			return runSetupUpdate(args[1:], stdout, stderr)
		default:
			fmt.Fprintf(stderr, "go-tmux-saver setup: unknown subcommand %q\n", args[0])
			fmt.Fprintln(stderr, "usage: go-tmux-saver setup generate|install|validate|update [flags]")
			return 2
		}
	}})
}
