// go-tmux-saver is Linux-only by design: it scans /proc for pane process
// trees (internal/procs) and flocks the data dir (internal/cli). The build
// constraint makes a non-Linux build fail here, at compile time, with a
// clear "build constraints exclude all Go files" message instead of
// misbehaving at runtime (issue #9). A future port starts by removing this
// tag and making internal/procs portable.

//go:build linux

package main

import (
	"os"

	"github.com/mithro/go-tmux-saver/internal/cli"
)

func main() { os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr)) }
