package cli

import (
	"context"
	"encoding/json"
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

// RULING R33: execCommand takes an explicit per-call timeout rather than
// baking in one shared bound — status's timerState must stay responsive
// (bounded at timerStateExecTimeout) even when a user systemd instance is
// hung or unreachable, independent of the more generous bound the
// interactive `setup` subcommands get (they can involve slower systemd
// operations, e.g. daemon-reload).
const (
	// setupExecTimeout bounds realSystemctl/realTmuxBindings — every
	// systemctl/systemd-analyze/tmux call `setup install|validate|update`
	// make.
	setupExecTimeout = 15 * time.Second
	// timerStateExecTimeout bounds realTimerState's `systemctl --user
	// is-active` check — `status` must not block long on a hung systemd.
	timerStateExecTimeout = 3 * time.Second
)

// execCommand is the seam every real subprocess call in this file goes
// through: run name(args...) to completion, bounded by timeout, and return
// its combined stdout+stderr. Tests swap this package var for a fake to
// assert both dispatch (systemctl vs. systemd-analyze, tmux list-keys) and
// the timeout each call site requests, without touching the real system.
var execCommand = func(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
	out, err := execCommand(setupExecTimeout, bin, args...)
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	return string(out), nil
}

// realTmuxBindings returns an Env.TmuxBindings implementation that lists
// key bindings from cfg's tmux socket.
func realTmuxBindings(cfg config.Config) func() (string, error) {
	return func() (string, error) {
		out, err := execCommand(setupExecTimeout, "tmux", "-L", cfg.Socket, "list-keys")
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
	dir := fs.String("dir", "", "directory to render managed files into (Rel-relative), without touching systemd/tmux; omit to print them to stdout instead")
	cfgPath := fs.String("config", config.Path(), "config file")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	_, files, code := renderSetupFiles(*cfgPath, stderr)
	if code != 0 {
		return code
	}

	// RULING R50: with no --dir, render to stdout rather than defaulting to
	// the real ConfigHome. "Show me what you would install" must not be a
	// command that installs; `setup install` is how you write to ConfigHome.
	if *dir == "" {
		for _, f := range files {
			fmt.Fprintf(stdout, "=== %s ===\n", f.Rel)
			stdout.Write(f.Content)
			if len(f.Content) > 0 && f.Content[len(f.Content)-1] != '\n' {
				fmt.Fprintln(stdout)
			}
		}
		return 0
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

// validateJSON is `setup validate --json`'s output shape (RULING R50):
// {"ok":bool,"drifts":[{"path":…,"kind":…,"diff":…}]}. Declared here rather
// than reusing setup.Drift directly so the wire format stays independent of
// that struct's Go field names.
type validateJSON struct {
	OK     bool              `json:"ok"`
	Drifts []validateDriftJS `json:"drifts"`
}

type validateDriftJS struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Diff string `json:"diff"`
}

func runSetupValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup validate", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit the drift report as JSON instead of text")
	cfgPath := fs.String("config", config.Path(), "config file")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, files, code := renderSetupFiles(*cfgPath, stderr)
	if code != 0 {
		return code
	}
	// Validate's own progress/diff output must never end up interleaved
	// with the JSON document on stdout.
	envOut := stdout
	if *asJSON {
		envOut = io.Discard
	}
	env := realSetupEnv(cfg, envOut)
	drifts := setup.Validate(env, files)

	if *asJSON {
		rep := validateJSON{OK: len(drifts) == 0, Drifts: []validateDriftJS{}}
		for _, d := range drifts {
			rep.Drifts = append(rep.Drifts, validateDriftJS{Path: d.Path, Kind: d.Kind, Diff: d.Diff})
		}
		b, err := json.Marshal(rep)
		if err != nil {
			fmt.Fprintln(stderr, "setup validate:", err)
			return 1
		}
		fmt.Fprintln(stdout, string(b))
		if len(drifts) == 0 {
			return 0
		}
		return 1
	}

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
// (bounded at timerStateExecTimeout, RULING R33) so tests can fake it. Any
// exec failure (systemctl missing, no user systemd instance, timeout,
// non-zero exit for e.g. "inactive"/"failed" reported as an error by
// is-active) reports "unknown" rather than propagating an error status has
// no way to show.
func realTimerState() string {
	out, err := execCommand(timerStateExecTimeout, "systemctl", "--user", "is-active", "go-tmux-saver.timer")
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
