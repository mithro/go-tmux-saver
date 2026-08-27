// Package cli implements the go-tmux-saver subcommands.
package cli

import (
	"fmt"
	"io"
	"path/filepath"
)

// Version is set at build time via -ldflags "-X .../internal/cli.Version=v1.2".
var Version = "dev"

type command struct {
	name string
	help string
	run  func(args []string, stdout, stderr io.Writer) int
}

var commands []command

func register(c command) { commands = append(commands, c) }

func init() {
	register(command{"version", "print version", func(_ []string, stdout, _ io.Writer) int {
		fmt.Fprintf(stdout, "go-tmux-saver %s\n", Version)
		return 0
	}})
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: go-tmux-saver <command> [flags]")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-18s %s\n", c.name, c.help)
	}
}

// RunMultiCall dispatches by the invoked name first (busybox-style):
// installed as a `claude-resume` symlink (setup manages ~/bin/claude-resume
// → this binary), the binary IS the placeholder — argv becomes
// `claude-resume <args...>` — otherwise it behaves exactly like Run.
func RunMultiCall(argv0 string, args []string, stdout, stderr io.Writer) int {
	if filepath.Base(argv0) == "claude-resume" {
		return Run(append([]string{"claude-resume"}, args...), stdout, stderr)
	}
	return Run(args, stdout, stderr)
}

// Run dispatches args[0] to a registered subcommand and returns the exit code.
// The conventional flag spellings people type first are accepted as aliases:
// --version/-v for the version subcommand, and --help/-h/help for usage
// (printed to stdout with exit 0 — asking for help is not an error).
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "--version", "-v":
		args = append([]string{"version"}, args[1:]...)
	case "--help", "-h", "help":
		usage(stdout)
		return 0
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "go-tmux-saver: unknown command %q\n", args[0])
	usage(stderr)
	return 2
}
