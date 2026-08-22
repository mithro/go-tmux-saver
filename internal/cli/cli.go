// Package cli implements the go-tmux-saver subcommands.
package cli

import (
	"fmt"
	"io"
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

// Run dispatches args[0] to a registered subcommand and returns the exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
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
