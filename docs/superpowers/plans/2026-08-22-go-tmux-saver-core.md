# go-tmux-saver Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the `go-tmux-saver` binary — fast, robust tmux save/restore over one control-mode connection, with guard, event log, hardlinked per-pane snapshots, additive restore, status/watchdog, self-managing systemd/tmux config (`setup`), alerting and a resurrect importer.

**Architecture:** One static Go binary. All tmux data comes over a single control-mode connection per invocation (`tmux -L <sock> -C attach-session -f no-output -t <session>`), all process data from one `/proc` pass plus Claude's per-pid registry. Snapshots are directories (`layout.json` + one compressed scrollback file per pane, hardlinked when unchanged) promoted atomically; a pure planner turns (live state, snapshot) into an additive command list that an applier executes. Systemd user units and the tmux keybinding snippet are rendered from embedded templates by `setup`.

**Tech Stack:** Go 1.26 (stdlib only: `os/exec`, `bufio`, `encoding/json`, `compress/gzip`, `crypto/sha256`, `embed`, `text/template`, `flag`, `testing`), tmux control mode, systemd user units, `sendmail -t`.

**Spec:** `docs/superpowers/specs/2026-08-22-go-tmux-saver-design.md`

This is **Plan 1 of 2**. Plan 2 (CI release pipeline, `.deb`/apt repo, rcfiles retirement of resurrect/continuum, ansible role, per-host rollout) is written after this plan lands.

## Global Constraints

- Module path `github.com/mithro/go-tmux-saver`; Go ≥ 1.26; `CGO_ENABLED=0`; **no third-party dependencies** in this plan (stdlib only).
- Must work against stock tmux ≥ 3.5a (control mode, `-f no-output`, `#{session_grouped}`), never rely on `mithro/tmux` fork features.
- Never fork per pane: exactly one control-mode connection per invocation and one `/proc` scan.
- Every written file: temp + fsync + rename; snapshot dirs promoted via `snap-<UTC>.tmp/` → rename; `last` symlink swapped atomically.
- Data dir mode `0700`, files `0600`.
- Snapshot schema `"schema": 1`; pane content files named `<session>_<window>_<pane>.txt<codec-ext>`; hash = sha256 of the **uncompressed** capture.
- Outcomes (exact strings): `kept`, `unchanged`, `rejected-degenerate`, `skipped`, `error`. `save --auto` exits 0 for all but `error`.
- Guard defaults: `guard.min_panes` 5, `guard.divisor` 3. Retention defaults: keep 50, daily 30 days, rejected 20. Interval default 10 min; stale factor 3.
- Restore is additive: existing windows are never renamed/moved/modified; conflicts relocate; `--on-start` only on a seed-only server (session `default`, single window `h`).
- Keybindings `M-s` → `run-shell "go-tmux-saver save"`, `M-r` → `run-shell "go-tmux-saver restore --merge"`.
- Claude placeholder command: `~/bin/claude-resume <uuid>` (configurable `claude_resume_path`).
- Commits: small, one per task step group, message style `feat(pkg): …` / `test(pkg): …` / `docs: …`; every commit carries the `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` trailer.
- TDD: every task writes the failing test first and runs it before implementing.
- Dates in docs/commits: ISO `YYYY-MM-DD`.

---

## File Structure

```
go.mod
cmd/go-tmux-saver/main.go                  CLI entry: subcommand dispatch, exit codes
internal/cli/                              one file per subcommand (save.go, restore.go, status.go, prune.go, setup.go, alert.go, importer.go)
internal/tmuxctl/parse.go                  control-mode framing parser (pure)
internal/tmuxctl/transport.go              Transport interface, Fake transport (transcript-backed)
internal/tmuxctl/client.go                 real Client over `tmux -C` subprocess
internal/tmuxctl/testdata/*.transcript     recorded control-mode sessions
internal/procs/procs.go                    /proc scan, Table, Subtree
internal/procs/claude.go                   Claude registry lookup
internal/procs/resolve.go                  pane → Restore resolution (allowlist)
internal/procs/testdata/proc/...           fake /proc trees
internal/snapshot/schema.go                Snapshot/Session/Window/Pane/Restore types, PaneKey
internal/snapshot/codec.go                 Codec interface + registry; gzip codec
internal/snapshot/store.go                 Store: Stage/Promote/Reject/Last/Load/ReadContent, hardlinks
internal/snapshot/guard.go                 IsDegenerate
internal/snapshot/events.go                events.log append/tail + freshness marker
internal/snapshot/prune.go                 retention
internal/collect/collect.go                build Snapshot+contents from Transport+procs
internal/collect/unchanged.go              Unchanged(a,b) comparison incl. title rule
internal/restore/live.go                   LiveState query + IsSeedOnly
internal/restore/plan.go                   pure planner
internal/restore/apply.go                  applier over Transport
internal/config/config.go                  config.json defaults/load/validate
internal/setup/templates/*.tmpl            unit/drop-in/tmux snippet templates (embedded)
internal/setup/render.go                   Params → []Managed
internal/setup/env.go                      Env (paths, injectable systemctl/tmux)
internal/setup/install.go                  Install / Update
internal/setup/validate.go                 Validate → []Drift
internal/mail/mail.go                      sendmail -t + rate limit
internal/importer/resurrect.go             import-resurrect
.github/workflows/test.yml                 go vet + go test (tmux installed for integration tests)
```

Naming used throughout (later tasks rely on these exact names):

```go
// internal/tmuxctl
type Reply struct { Lines []string; Err bool }
type Transport interface { Run(ctx context.Context, cmd string) ([]string, error); Close() error }
func ParseReplies(r io.Reader, out chan<- Reply) error
func Dial(ctx context.Context, socket, session string) (*Client, error)
type Fake struct { Replies map[string][]string; Calls []string }

// internal/procs
type Proc struct { PID, PPID int; Comm string; Cmdline []string; StartTime string }
type Table struct { /* unexported */ }
func Scan(procRoot string) (*Table, error)
func (t *Table) Get(pid int) (Proc, bool)
func (t *Table) Subtree(pid int) []int
type ClaudeRegistry struct { Dir string }
func (r ClaudeRegistry) SessionFor(p Proc) (string, bool)
func Resolve(t *Table, reg ClaudeRegistry, panePID int, allowlist []string) snapshot.Restore

// internal/snapshot
type Snapshot, Session, Window, Pane, Restore, ClientState, Stats
func PaneKey(session string, window, pane int) string
type Codec interface { Name() string; Ext() string; NewWriter(io.Writer) (io.WriteCloser, error); NewReader(io.Reader) (io.ReadCloser, error) }
func RegisterCodec(c Codec); func LookupCodec(name string) (Codec, bool)
type Store struct { Dir string; Codec Codec }
func (s *Store) Stage(snap *Snapshot, contents map[string][]byte) (*Staged, error)
func (st *Staged) Promote() (string, error); func (st *Staged) Reject() (string, error); func (st *Staged) Discard() error
func (s *Store) Last() (*Snapshot, string, error)
func (s *Store) Load(dir string) (*Snapshot, error)
func (s *Store) ReadContent(dir string, p Pane) ([]byte, error)
func IsDegenerate(newPanes, lastPanes, minPanes, divisor int) bool
type Event struct {...}; func AppendEvent(dir string, e Event) error; func TailEvents(dir string, n int) ([]Event, error); func TouchFresh(dir string) error; func LastGood(dir string) (time.Time, bool, error)
func Prune(dir string, keep, dailyDays, rejectedKeep int, now time.Time) (removed []string, err error)

// internal/collect
type Collector struct { T tmuxctl.Transport; Procs *procs.Table; Reg procs.ClaudeRegistry; Allowlist []string; Host string; Now func() time.Time }
func (c *Collector) Collect(ctx context.Context) (*snapshot.Snapshot, map[string][]byte, error)
func Unchanged(a, b *snapshot.Snapshot) bool

// internal/restore
type LiveWindow struct { Index int; Name string }
type LiveState struct { Sessions map[string][]LiveWindow; Clients int }
func QueryLive(ctx context.Context, t tmuxctl.Transport) (LiveState, error)
func IsSeedOnly(l LiveState, seedSession, seedWindow string) bool
type Action struct { Kind string; Args []string; Note string }   // Kind: tmux | note
type Plan struct { Actions []Action; Created, Relocated, Skipped int }
func BuildPlan(live LiveState, snap *snapshot.Snapshot, o Options) Plan
func Apply(ctx context.Context, t tmuxctl.Transport, p Plan, contents func(snapshot.Pane) ([]byte, bool)) (Report, error)
```

---

### Task 1: Module scaffold, CLI skeleton, CI test workflow

**Files:**
- Create: `go.mod`, `cmd/go-tmux-saver/main.go`, `internal/cli/cli.go`, `internal/cli/cli_test.go`, `.github/workflows/test.yml`, `README.md`

**Interfaces:**
- Produces: `cli.Run(args []string, stdout, stderr io.Writer) int` — dispatches subcommands; unknown → usage + exit 2. `cli.Version` var set via `-ldflags "-X github.com/mithro/go-tmux-saver/internal/cli.Version=…"`.

- [ ] **Step 1: Write the failing test**

`internal/cli/cli_test.go`:
```go
package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var out, errb bytes.Buffer
	Version = "v0.0-test"
	if code := Run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "go-tmux-saver v0.0-test") {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestRunUnknown(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"bogus"}, &out, &errb); code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
	if !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("want usage on stderr, got %q", errb.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/github/mithro/go-tmux-saver && go mod init github.com/mithro/go-tmux-saver && go test ./internal/cli/ -run 'TestRun' -v`
Expected: FAIL — `undefined: Run`, `undefined: Version`.

- [ ] **Step 3: Write minimal implementation**

`internal/cli/cli.go`:
```go
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
```

`cmd/go-tmux-saver/main.go`:
```go
package main

import (
	"os"

	"github.com/mithro/go-tmux-saver/internal/cli"
)

func main() { os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr)) }
```

`.github/workflows/test.yml`:
```yaml
name: test
on:
  push:
    branches: [main]
  pull_request:
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: sudo apt-get update && sudo apt-get install -y tmux
      - run: go vet ./...
      - run: go test -race ./...
```

`README.md` — three lines: name, one-sentence purpose, pointer to the spec and plan under `docs/superpowers/`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -v && go build ./cmd/go-tmux-saver && ./go-tmux-saver version`
Expected: PASS; prints `go-tmux-saver dev`.

- [ ] **Step 5: Commit**

```bash
git add go.mod cmd internal/cli .github README.md
git commit -m "feat: module scaffold, CLI dispatch, version subcommand, CI test workflow"
```

---

### Task 2: Control-mode framing parser (pure)

**Files:**
- Create: `internal/tmuxctl/parse.go`, `internal/tmuxctl/parse_test.go`, `internal/tmuxctl/testdata/probe.transcript`

**Interfaces:**
- Produces: `type Reply struct { Lines []string; Err bool }`; `func ParseReplies(r io.Reader, out chan<- Reply) error` — reads control-mode output, emits one `Reply` per `%begin…%end|%error` block (lines between, excluding `%`-notifications *inside* a block such as `%session-changed`), ignores notifications outside blocks, returns `nil` on `%exit` or EOF, and closes nothing (caller owns the channel).

- [ ] **Step 1: Write the failing test**

`internal/tmuxctl/testdata/probe.transcript` (verbatim from the 2026-08-22 probe; escape bytes written as `\x1b` are real ESC bytes in the file — create it with `printf`):
```
%begin 1787376176 441 0
%session-changed $0 probe
%end 1787376176 441 0
%begin 1787376176 444 1
probe
%end 1787376176 444 1
%begin 1787376176 445 1
probe	0	0	%0	1683945	0
%end 1787376176 445 1
%begin 1787376176 446 1
\x1b[1m\x1b[32mtim@ten64\x1b[0m:\x1b[1m\x1b[34m~\x1b[0m$ 


%end 1787376176 446 1
%exit
```

`internal/tmuxctl/parse_test.go`:
```go
package tmuxctl

import (
	"os"
	"strings"
	"testing"
)

func collect(t *testing.T, input string) []Reply {
	t.Helper()
	ch := make(chan Reply, 16)
	if err := ParseReplies(strings.NewReader(input), ch); err != nil {
		t.Fatal(err)
	}
	close(ch)
	var out []Reply
	for r := range ch {
		out = append(out, r)
	}
	return out
}

func TestParseProbeTranscript(t *testing.T) {
	data, err := os.ReadFile("testdata/probe.transcript")
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, string(data))
	if len(got) != 4 {
		t.Fatalf("want 4 replies, got %d: %+v", len(got), got)
	}
	if len(got[0].Lines) != 0 { // attach block contains only a %session-changed notification
		t.Errorf("attach reply should have no data lines, got %q", got[0].Lines)
	}
	if got[1].Lines[0] != "probe" {
		t.Errorf("list-sessions reply = %q", got[1].Lines)
	}
	if !strings.HasPrefix(got[2].Lines[0], "probe\t0\t0\t%0\t") {
		t.Errorf("list-panes reply = %q", got[2].Lines)
	}
	if len(got[3].Lines) != 3 || !strings.Contains(got[3].Lines[0], "tim@ten64") {
		t.Errorf("capture-pane reply = %q", got[3].Lines)
	}
}

func TestParseErrorBlock(t *testing.T) {
	got := collect(t, "%begin 1 2 1\nno such session: x\n%error 1 2 1\n%exit\n")
	if len(got) != 1 || !got[0].Err || got[0].Lines[0] != "no such session: x" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseMismatchedEndIsData(t *testing.T) {
	// an %end whose number does not match the open %begin is pane data, not a terminator
	got := collect(t, "%begin 1 5 1\n%end 1 999 1\nreal\n%end 1 5 1\n")
	if len(got) != 1 || len(got[0].Lines) != 2 || got[0].Lines[0] != "%end 1 999 1" {
		t.Fatalf("got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tmuxctl/ -run TestParse -v`
Expected: FAIL — `undefined: ParseReplies`, `undefined: Reply`.

- [ ] **Step 3: Write minimal implementation**

`internal/tmuxctl/parse.go`:
```go
// Package tmuxctl talks to a tmux server over a single control-mode connection.
package tmuxctl

import (
	"bufio"
	"io"
	"strings"
)

// Reply is the body of one %begin…%end / %error block.
type Reply struct {
	Lines []string
	Err   bool
}

// ParseReplies reads control-mode output from r and sends one Reply per
// command block on out. Notifications outside a block (%exit, %layout-change…)
// are ignored; a %session-changed/%window-* notification inside a block is
// dropped from the body. Terminators are matched by block number so a pane
// line that merely looks like "%end …" cannot close the block early.
// Returns nil at %exit or EOF.
func ParseReplies(r io.Reader, out chan<- Reply) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var (
		inBlock bool
		num     string
		cur     Reply
	)
	for sc.Scan() {
		line := sc.Text()
		if !inBlock {
			if strings.HasPrefix(line, "%begin ") {
				f := strings.Fields(line)
				if len(f) >= 3 {
					inBlock, num, cur = true, f[2], Reply{}
				}
				continue
			}
			if line == "%exit" {
				return nil
			}
			continue // other notification
		}
		if strings.HasPrefix(line, "%end ") || strings.HasPrefix(line, "%error ") {
			f := strings.Fields(line)
			if len(f) >= 3 && f[2] == num {
				cur.Err = f[0] == "%error"
				out <- cur
				inBlock = false
				continue
			}
		}
		if strings.HasPrefix(line, "%") && isNotification(line) {
			continue
		}
		cur.Lines = append(cur.Lines, line)
	}
	return sc.Err()
}

func isNotification(line string) bool {
	for _, p := range []string{"%session-changed", "%sessions-changed", "%window-add",
		"%window-close", "%window-renamed", "%layout-change", "%unlinked-window-",
		"%client-session-changed", "%client-detached", "%pane-mode-changed", "%subscription-changed"} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tmuxctl/ -run TestParse -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/tmuxctl
git commit -m "feat(tmuxctl): control-mode reply framing parser with recorded probe fixture"
```

---

### Task 3: Transport interface, Fake transport, real Client

**Files:**
- Create: `internal/tmuxctl/transport.go`, `internal/tmuxctl/client.go`, `internal/tmuxctl/client_test.go`, `internal/tmuxctl/fake_test.go`, `internal/tmuxctl/testutil.go`

**Interfaces:**
- Produces: `Transport` interface; `Fake` (map command→lines, records `Calls`, returns error for unknown command unless `Fake.Default` set); `Dial(ctx, socket, session) (*Client, error)`; `(*Client).Run(ctx, cmd) ([]string, error)` (error of type `*CmdError{Cmd, Lines}` on `%error`); `(*Client).Close()`; test helper `StartTestServer(t) (socket string)` which runs `tmux -L <sock> new-session -d -s default -n h "tail -f /dev/null"` and kills it on cleanup, skipping the test if `tmux` is not on PATH.

- [ ] **Step 1: Write the failing tests**

`internal/tmuxctl/fake_test.go`:
```go
package tmuxctl

import (
	"context"
	"testing"
)

func TestFakeRecordsCallsAndReplies(t *testing.T) {
	f := &Fake{Replies: map[string][]string{"list-sessions": {"a", "b"}}}
	got, err := f.Run(context.Background(), "list-sessions")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := f.Run(context.Background(), "nope"); err == nil {
		t.Fatal("unknown command should error")
	}
	if len(f.Calls) != 2 || f.Calls[0] != "list-sessions" {
		t.Fatalf("calls = %v", f.Calls)
	}
}
```

`internal/tmuxctl/client_test.go`:
```go
package tmuxctl

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestClientRoundTrip(t *testing.T) {
	sock := StartTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, sock, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	lines, err := c.Run(ctx, `list-windows -t default -F "#{window_index}\t#{window_name}"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "0\th" {
		t.Fatalf("list-windows = %q", lines)
	}
	if _, err := c.Run(ctx, "list-windows -t nosuchsession"); err == nil {
		t.Fatal("expected %error for bad target")
	} else if !strings.Contains(err.Error(), "nosuchsession") {
		t.Fatalf("error should carry tmux message, got %v", err)
	}
	// second command after an error still works on the same connection
	if _, err := c.Run(ctx, "list-sessions"); err != nil {
		t.Fatal(err)
	}
}

func TestClientContextTimeout(t *testing.T) {
	sock := StartTestServer(t)
	c, err := Dial(context.Background(), sock, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if _, err := c.Run(ctx, "list-sessions"); err == nil {
		t.Fatal("expected context deadline error")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tmuxctl/ -run 'TestFake|TestClient' -v`
Expected: FAIL — `undefined: Fake`, `undefined: Dial`, `undefined: StartTestServer`.

- [ ] **Step 3: Write minimal implementation**

`internal/tmuxctl/transport.go`:
```go
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
```

`internal/tmuxctl/client.go`:
```go
package tmuxctl

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// Client is a live control-mode connection to one tmux server.
type Client struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	replies chan Reply
	parseErr chan error
	mu      sync.Mutex
}

// Dial starts `tmux -L socket -C attach-session -f no-output -t session` and
// consumes the initial attach block. `-f no-output` stops pane output
// notifications flooding the connection.
func Dial(ctx context.Context, socket, session string) (*Client, error) {
	cmd := exec.Command("tmux", "-L", socket, "-C", "attach-session", "-f", "no-output", "-t", session)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start tmux control client: %w", err)
	}
	c := &Client{cmd: cmd, stdin: stdin, replies: make(chan Reply, 64), parseErr: make(chan error, 1)}
	go func() {
		c.parseErr <- ParseReplies(stdout, c.replies)
		close(c.replies)
	}()
	// initial attach block
	if _, err := c.next(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("attach to %s on socket %s: %w", session, socket, err)
	}
	return c, nil
}

func (c *Client) next(ctx context.Context) (Reply, error) {
	select {
	case r, ok := <-c.replies:
		if !ok {
			return Reply{}, fmt.Errorf("control connection closed")
		}
		return r, nil
	case <-ctx.Done():
		return Reply{}, ctx.Err()
	}
}

// Run sends one command and returns its reply lines. Commands are serialised.
func (c *Client) Run(ctx context.Context, cmd string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
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
```

`internal/tmuxctl/testutil.go`:
```go
package tmuxctl

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

// StartTestServer starts a throwaway tmux server (session "default", window
// "h") on a unique socket and kills it when the test ends. Skips if tmux is
// not installed.
func StartTestServer(t testing.TB) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sock := fmt.Sprintf("gts-test-%d-%s", os.Getpid(), t.Name())
	if out, err := exec.Command("tmux", "-L", sock, "new-session", "-d", "-s", "default", "-n", "h", "tail -f /dev/null").CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("tmux", "-L", sock, "kill-server").Run() })
	return sock
}
```

Note for `sock`: `t.Name()` may contain `/`; sanitise by replacing `/` with `_` before use (add `strings.ReplaceAll`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tmuxctl/ -v`
Expected: PASS (parser, fake, client round-trip, timeout).

- [ ] **Step 5: Commit**

```bash
git add internal/tmuxctl
git commit -m "feat(tmuxctl): Transport interface, transcript Fake, live control-mode Client with test server helper"
```

---

### Task 4: `/proc` table, subtree and Claude registry

**Files:**
- Create: `internal/procs/procs.go`, `internal/procs/claude.go`, `internal/procs/procs_test.go`, `internal/procs/testdata/proc/{100,101,102,200}/{stat,cmdline}`, `internal/procs/testdata/sessions/101.json`

**Interfaces:**
- Produces: `Proc{PID, PPID int; Comm string; Cmdline []string; StartTime string}`; `Scan(procRoot string) (*Table, error)`; `(*Table).Get(pid) (Proc, bool)`; `(*Table).Subtree(pid) []int` (BFS, root first); `ClaudeRegistry{Dir string}`; `(ClaudeRegistry).SessionFor(p Proc) (sessionID string, ok bool)` — reads `<Dir>/<pid>.json`, requires `sessionId` and (if present) `procStart == p.StartTime`.

- [ ] **Step 1: Write the failing tests**

Fixture files (`testdata/proc/<pid>/stat` uses real format: `pid (comm) S ppid …` with 52 fields; starttime is field 22):

- `100/stat`: `100 (bash) S 1 100 100 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 5000 0 0 ...` (pad to 52 fields with zeros)
- `100/cmdline`: `bash\0`
- `101/stat`: `101 (claude) S 100 … starttime 6000 …`; `101/cmdline`: `claude\0`
- `102/stat`: `102 (tail) S 101 … 7000 …`; `102/cmdline`: `tail\0-f\0x\0`
- `200/stat`: `200 (python3) S 1 … 8000 …`; `200/cmdline`: `python3\0/home/tim/bin/claude-resume\0aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee\0`
- `testdata/sessions/101.json`: `{"pid":101,"sessionId":"11111111-2222-3333-4444-555555555555","procStart":"6000"}`

`internal/procs/procs_test.go`:
```go
package procs

import "testing"

func TestScanAndSubtree(t *testing.T) {
	tb, err := Scan("testdata/proc")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := tb.Get(101)
	if !ok || p.Comm != "claude" || p.PPID != 100 || p.StartTime != "6000" {
		t.Fatalf("proc 101 = %+v ok=%v", p, ok)
	}
	p200, _ := tb.Get(200)
	if len(p200.Cmdline) != 3 || p200.Cmdline[1] != "/home/tim/bin/claude-resume" {
		t.Fatalf("cmdline = %q", p200.Cmdline)
	}
	got := tb.Subtree(100)
	want := []int{100, 101, 102}
	if len(got) != 3 || got[0] != 100 || got[1] != 101 || got[2] != 102 {
		t.Fatalf("subtree = %v want %v", got, want)
	}
}

func TestClaudeRegistry(t *testing.T) {
	reg := ClaudeRegistry{Dir: "testdata/sessions"}
	tb, _ := Scan("testdata/proc")
	p, _ := tb.Get(101)
	sid, ok := reg.SessionFor(p)
	if !ok || sid != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("got %q %v", sid, ok)
	}
	p.StartTime = "9999" // pid reused by a different process
	if _, ok := reg.SessionFor(p); ok {
		t.Fatal("stale procStart must not match")
	}
	if _, ok := reg.SessionFor(Proc{PID: 777}); ok {
		t.Fatal("missing registry file must not match")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/procs/ -v`
Expected: FAIL — `undefined: Scan`, `ClaudeRegistry`.

- [ ] **Step 3: Write minimal implementation**

`internal/procs/procs.go`:
```go
// Package procs reads the process table once and resolves pane processes.
package procs

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Proc struct {
	PID, PPID int
	Comm      string
	Cmdline   []string
	StartTime string // /proc/<pid>/stat field 22, opaque string
}

type Table struct {
	byPID    map[int]Proc
	children map[int][]int
}

// Scan reads every numeric directory under procRoot ("/proc" in production).
func Scan(procRoot string) (*Table, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	t := &Table{byPID: map[int]Proc{}, children: map[int][]int{}}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "stat"))
		if err != nil {
			continue // process exited mid-scan
		}
		p, ok := parseStat(pid, stat)
		if !ok {
			continue
		}
		if cl, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "cmdline")); err == nil {
			for _, part := range bytes.Split(bytes.TrimRight(cl, "\x00"), []byte{0}) {
				p.Cmdline = append(p.Cmdline, string(part))
			}
		}
		t.byPID[pid] = p
		t.children[p.PPID] = append(t.children[p.PPID], pid)
	}
	return t, nil
}

// parseStat handles comm containing spaces/parens by splitting on the LAST ')'.
func parseStat(pid int, stat []byte) (Proc, bool) {
	s := string(stat)
	lp, rp := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if lp < 0 || rp < 0 || rp < lp {
		return Proc{}, false
	}
	rest := strings.Fields(s[rp+1:]) // rest[0]=state rest[1]=ppid … rest[19]=starttime
	if len(rest) < 20 {
		return Proc{}, false
	}
	ppid, err := strconv.Atoi(rest[1])
	if err != nil {
		return Proc{}, false
	}
	return Proc{PID: pid, PPID: ppid, Comm: s[lp+1 : rp], StartTime: rest[19]}, true
}

func (t *Table) Get(pid int) (Proc, bool) { p, ok := t.byPID[pid]; return p, ok }

// Subtree returns pid and all descendants, breadth-first (shallowest first),
// children in ascending pid order for determinism.
func (t *Table) Subtree(pid int) []int {
	out := []int{pid}
	for i := 0; i < len(out); i++ {
		kids := append([]int(nil), t.children[out[i]]...)
		sortInts(kids)
		out = append(out, kids...)
	}
	return out
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}
```

`internal/procs/claude.go`:
```go
package procs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

// ClaudeRegistry reads Claude Code's per-pid session files
// (~/.claude/sessions/<pid>.json).
type ClaudeRegistry struct{ Dir string }

type registryEntry struct {
	SessionID string          `json:"sessionId"`
	ProcStart json.RawMessage `json:"procStart"`
}

// SessionFor returns the session id recorded for p, validated against the
// process start time so a reused pid cannot match a stale entry.
func (r ClaudeRegistry) SessionFor(p Proc) (string, bool) {
	data, err := os.ReadFile(filepath.Join(r.Dir, strconv.Itoa(p.PID)+".json"))
	if err != nil {
		return "", false
	}
	var e registryEntry
	if json.Unmarshal(data, &e) != nil || e.SessionID == "" {
		return "", false
	}
	if len(e.ProcStart) > 0 {
		var asStr string
		var asNum json.Number
		switch {
		case json.Unmarshal(e.ProcStart, &asStr) == nil:
			if asStr != p.StartTime {
				return "", false
			}
		case json.Unmarshal(e.ProcStart, &asNum) == nil:
			if asNum.String() != p.StartTime {
				return "", false
			}
		}
	}
	return e.SessionID, true
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/procs/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/procs
git commit -m "feat(procs): single-pass /proc table, BFS subtree, Claude registry lookup with stale-pid guard"
```

---

### Task 5: Snapshot schema, PaneKey, codec registry (gzip)

**Files:**
- Create: `internal/snapshot/schema.go`, `internal/snapshot/codec.go`, `internal/snapshot/schema_test.go`, `internal/snapshot/codec_test.go`

**Interfaces:**
- Produces (exact):
```go
type Snapshot struct {
	Schema        int         `json:"schema"`
	Host          string      `json:"host"`
	TmuxVersion   string      `json:"tmux_version"`
	TakenAt       time.Time   `json:"taken_at"`
	ServerStart   int64       `json:"server_start"`
	ContentsCodec string      `json:"contents_codec"`
	Sessions      []Session   `json:"sessions"`
	Client        ClientState `json:"client"`
	Stats         Stats       `json:"stats"`
}
type Session struct { Name string `json:"name"`; ActiveWindow int `json:"active_window"`; Windows []Window `json:"windows"` }
type Window struct { Index int `json:"index"`; Name string `json:"name"`; Layout string `json:"layout"`; Active bool `json:"active"`; Flags string `json:"flags"`; AutomaticRename bool `json:"automatic_rename"`; Panes []Pane `json:"panes"` }
type Pane struct { Index int `json:"index"`; ID string `json:"id"`; Cwd string `json:"cwd"`; Title string `json:"title"`; Active bool `json:"active"`; HistoryLines int `json:"history_lines"`; ContentSHA256 string `json:"content_sha256,omitempty"`; ContentFile string `json:"content_file,omitempty"`; Restore Restore `json:"restore"` }
type Restore struct { Kind string `json:"kind"`; Argv []string `json:"argv,omitempty"`; ClaudeSession string `json:"claude_session,omitempty"` } // Kind: "shell" | "argv" | "claude"
type ClientState struct { Session string `json:"session"` }
type Stats struct { Panes, Windows, Sessions int; DurationMS int64 `json:"duration_ms"` }
const SchemaVersion = 1
func PaneKey(session string, window, pane int) string   // "session_window_pane"; "/" and whitespace in session → "-"
func (s *Snapshot) CountPanes() (panes, windows int)
type Codec interface { Name() string; Ext() string; NewWriter(io.Writer) (io.WriteCloser, error); NewReader(io.Reader) (io.ReadCloser, error) }
func RegisterCodec(c Codec); func LookupCodec(name string) (Codec, bool)   // "gzip" registered in init(), Ext ".gz"
```

- [ ] **Step 1: Write the failing tests**

`internal/snapshot/schema_test.go`:
```go
package snapshot

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSchemaRoundTrip(t *testing.T) {
	s := &Snapshot{Schema: SchemaVersion, Host: "h", TakenAt: time.Unix(1, 0).UTC(),
		Sessions: []Session{{Name: "net", Windows: []Window{{Index: 2, Name: "w",
			Panes: []Pane{{Index: 0, ID: "%5", Cwd: "/tmp", Restore: Restore{Kind: "argv", Argv: []string{"ssh", "host"}}}}}}}}}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back Snapshot
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Sessions[0].Windows[0].Panes[0].Restore.Argv[1] != "host" || back.Schema != 1 {
		t.Fatalf("round trip lost data: %+v", back)
	}
	p, w := back.CountPanes()
	if p != 1 || w != 1 {
		t.Fatalf("counts %d %d", p, w)
	}
}

func TestPaneKey(t *testing.T) {
	if got := PaneKey("net", 2, 0); got != "net_2_0" {
		t.Fatal(got)
	}
	if got := PaneKey("a b/c", 1, 1); got != "a-b-c_1_1" {
		t.Fatal(got)
	}
}
```

`internal/snapshot/codec_test.go`:
```go
package snapshot

import (
	"bytes"
	"io"
	"testing"
)

func TestGzipCodecRoundTrip(t *testing.T) {
	c, ok := LookupCodec("gzip")
	if !ok || c.Ext() != ".gz" {
		t.Fatalf("gzip codec missing or wrong ext: %v %v", c, ok)
	}
	var buf bytes.Buffer
	w, err := c.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "hello scrollback")
	w.Close()
	r, err := c.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	if string(got) != "hello scrollback" {
		t.Fatalf("got %q", got)
	}
	if _, ok := LookupCodec("zstd"); ok {
		t.Fatal("zstd must not be registered yet")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/snapshot/ -v`
Expected: FAIL — undefined types.

- [ ] **Step 3: Write minimal implementation**

`internal/snapshot/schema.go` — the types exactly as listed in Interfaces, plus:
```go
const SchemaVersion = 1

var keyUnsafe = regexp.MustCompile(`[\s/]+`)

// PaneKey is the structural, server-restart-stable name of a pane.
func PaneKey(session string, window, pane int) string {
	return fmt.Sprintf("%s_%d_%d", keyUnsafe.ReplaceAllString(session, "-"), window, pane)
}

func (s *Snapshot) CountPanes() (panes, windows int) {
	for _, se := range s.Sessions {
		windows += len(se.Windows)
		for _, w := range se.Windows {
			panes += len(w.Panes)
		}
	}
	return
}
```

`internal/snapshot/codec.go`:
```go
package snapshot

import (
	"compress/gzip"
	"io"
	"sync"
)

// Codec compresses per-pane scrollback files. Pluggable so zstd etc. can be
// added later without a format break (the snapshot records the codec name).
type Codec interface {
	Name() string
	Ext() string
	NewWriter(w io.Writer) (io.WriteCloser, error)
	NewReader(r io.Reader) (io.ReadCloser, error)
}

var (
	codecMu sync.RWMutex
	codecs  = map[string]Codec{}
)

func RegisterCodec(c Codec) { codecMu.Lock(); codecs[c.Name()] = c; codecMu.Unlock() }

func LookupCodec(name string) (Codec, bool) {
	codecMu.RLock()
	defer codecMu.RUnlock()
	c, ok := codecs[name]
	return c, ok
}

type gzipCodec struct{}

func (gzipCodec) Name() string { return "gzip" }
func (gzipCodec) Ext() string  { return ".gz" }
func (gzipCodec) NewWriter(w io.Writer) (io.WriteCloser, error) {
	return gzip.NewWriterLevel(w, gzip.BestSpeed)
}
func (gzipCodec) NewReader(r io.Reader) (io.ReadCloser, error) { return gzip.NewReader(r) }

func init() { RegisterCodec(gzipCodec{}) }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/snapshot/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/snapshot
git commit -m "feat(snapshot): schema v1 types, structural PaneKey, pluggable codec registry with gzip"
```

---

### Task 6: Store — staging, hardlinks, atomic promote, reject, last, read

**Files:**
- Create: `internal/snapshot/store.go`, `internal/snapshot/store_test.go`

**Interfaces:**
- Produces: `Store{Dir string; Codec Codec}`; `(*Store).Stage(snap, contents map[paneKey][]byte) (*Staged, error)` — fills `snap.ContentSHA256/ContentFile` per pane, writes `layout.json` + `panes/*` into `<Dir>/snap-<UTC>.tmp/` hardlinking against `Last()` when the hash for the same PaneKey matches; `(*Staged).Promote() (dir string, err error)` renames to `snap-<UTC>/`, swaps `last`; `(*Staged).Reject() (dir, err)` renames under `rejected/`; `(*Staged).Discard()`; `(*Store).Last() (*Snapshot, dir string, err error)` (`os.ErrNotExist` when none); `(*Store).Load(dir)`; `(*Store).ReadContent(dir, pane) ([]byte, error)`; `(*Store).EnsureDir() error` creates `Dir` 0700 and removes stale `*.tmp`.
- Timestamp format for dir names: `20060102T150405Z` (UTC) — `Stage` takes it from `snap.TakenAt`.

- [ ] **Step 1: Write the failing tests**

`internal/snapshot/store_test.go`:
```go
package snapshot

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func twoPaneSnap(ts time.Time) *Snapshot {
	return &Snapshot{Schema: 1, TakenAt: ts, Sessions: []Session{{Name: "s", Windows: []Window{{Index: 0, Name: "w",
		Panes: []Pane{{Index: 0, ID: "%1", Restore: Restore{Kind: "shell"}}, {Index: 1, ID: "%2", Restore: Restore{Kind: "shell"}}}}}}}}
}

func nlink(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(fi.Sys().(*syscall.Stat_t).Nlink)
}

func TestStagePromoteHardlinkAndLast(t *testing.T) {
	gz, _ := LookupCodec("gzip")
	st := &Store{Dir: t.TempDir(), Codec: gz}
	if err := st.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Last(); !os.IsNotExist(err) {
		t.Fatalf("empty store Last err = %v", err)
	}
	t1 := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	c1 := map[string][]byte{"s_0_0": []byte("AAA"), "s_0_1": []byte("BBB")}
	stg, err := st.Stage(twoPaneSnap(t1), c1)
	if err != nil {
		t.Fatal(err)
	}
	dir1, err := stg.Promote()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir1) != "snap-20260822T100000Z" {
		t.Fatalf("dir name %s", dir1)
	}
	if _, err := os.Stat(filepath.Join(dir1, "layout.json")); err != nil {
		t.Fatal(err)
	}
	last, lastDir, err := st.Last()
	if err != nil || lastDir != dir1 || last.Sessions[0].Windows[0].Panes[0].ContentSHA256 == "" {
		t.Fatalf("Last = %+v %s %v", last, lastDir, err)
	}
	got, err := st.ReadContent(dir1, last.Sessions[0].Windows[0].Panes[1])
	if err != nil || string(got) != "BBB" {
		t.Fatalf("ReadContent = %q %v", got, err)
	}

	// second snapshot: pane 0 unchanged (→ hardlink), pane 1 changed (→ new file)
	t2 := t1.Add(10 * time.Minute)
	c2 := map[string][]byte{"s_0_0": []byte("AAA"), "s_0_1": []byte("CCC")}
	stg2, _ := st.Stage(twoPaneSnap(t2), c2)
	dir2, _ := stg2.Promote()
	f0 := filepath.Join(dir2, "panes", "s_0_0.txt.gz")
	if nlink(t, f0) != 2 {
		t.Fatalf("unchanged pane should be hardlinked (nlink=2), got %d", nlink(t, f0))
	}
	if nlink(t, filepath.Join(dir2, "panes", "s_0_1.txt.gz")) != 1 {
		t.Fatal("changed pane must be a fresh file")
	}
	// removing the old snapshot keeps the shared file alive
	os.RemoveAll(dir1)
	if got, _ := st.ReadContent(dir2, last.Sessions[0].Windows[0].Panes[0]); string(got) != "AAA" {
		t.Fatalf("content lost after pruning old dir: %q", got)
	}
	if _, lastDir, _ = st.Last(); lastDir != dir2 {
		t.Fatal("last not updated")
	}
	if fi, _ := os.Stat(st.Dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode().Perm())
	}
}

func TestRejectAndDiscardLeaveLastAlone(t *testing.T) {
	gz, _ := LookupCodec("gzip")
	st := &Store{Dir: t.TempDir(), Codec: gz}
	st.EnsureDir()
	t1 := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	stg, _ := st.Stage(twoPaneSnap(t1), map[string][]byte{"s_0_0": []byte("A"), "s_0_1": []byte("B")})
	dir1, _ := stg.Promote()
	stg2, _ := st.Stage(twoPaneSnap(t1.Add(time.Minute)), map[string][]byte{})
	rdir, err := stg2.Reject()
	if err != nil || filepath.Dir(rdir) != filepath.Join(st.Dir, "rejected") {
		t.Fatalf("reject = %s %v", rdir, err)
	}
	stg3, _ := st.Stage(twoPaneSnap(t1.Add(2*time.Minute)), map[string][]byte{})
	if err := stg3.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, lastDir, _ := st.Last(); lastDir != dir1 {
		t.Fatal("reject/discard must not move last")
	}
	if m, _ := filepath.Glob(filepath.Join(st.Dir, "*.tmp")); len(m) != 0 {
		t.Fatalf("tmp dirs left: %v", m)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/snapshot/ -run 'TestStage|TestReject' -v`
Expected: FAIL — `undefined: Store`.

- [ ] **Step 3: Write minimal implementation**

`internal/snapshot/store.go`:
```go
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const dirTimeFormat = "20060102T150405Z"

type Store struct {
	Dir   string
	Codec Codec
}

type Staged struct {
	store  *Store
	tmpDir string
	name   string // snap-<ts>
}

func (s *Store) EnsureDir() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil {
		return err
	}
	os.MkdirAll(filepath.Join(s.Dir, "rejected"), 0o700)
	stale, _ := filepath.Glob(filepath.Join(s.Dir, "snap-*.tmp"))
	for _, d := range stale {
		os.RemoveAll(d)
	}
	return nil
}

// Stage writes snap + contents into snap-<ts>.tmp/, hardlinking any pane whose
// content hash equals the same PaneKey's hash in the current last snapshot.
func (s *Store) Stage(snap *Snapshot, contents map[string][]byte) (*Staged, error) {
	if s.Codec == nil {
		return nil, errors.New("store: nil codec")
	}
	snap.ContentsCodec = s.Codec.Name()
	name := "snap-" + snap.TakenAt.UTC().Format(dirTimeFormat)
	tmp := filepath.Join(s.Dir, name+".tmp")
	os.RemoveAll(tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "panes"), 0o700); err != nil {
		return nil, err
	}
	prev, prevDir, err := s.Last()
	prevHash := map[string]Pane{}
	if err == nil {
		for _, se := range prev.Sessions {
			for _, w := range se.Windows {
				for _, p := range w.Panes {
					prevHash[PaneKey(se.Name, w.Index, p.Index)] = p
				}
			}
		}
	}
	for si := range snap.Sessions {
		se := &snap.Sessions[si]
		for wi := range se.Windows {
			w := &se.Windows[wi]
			for pi := range w.Panes {
				p := &w.Panes[pi]
				key := PaneKey(se.Name, w.Index, p.Index)
				data, ok := contents[key]
				if !ok {
					continue
				}
				sum := sha256.Sum256(data)
				p.ContentSHA256 = hex.EncodeToString(sum[:])
				p.ContentFile = key + ".txt" + s.Codec.Ext()
				dst := filepath.Join(tmp, "panes", p.ContentFile)
				if pp, ok := prevHash[key]; ok && pp.ContentSHA256 == p.ContentSHA256 && pp.ContentFile == p.ContentFile {
					if os.Link(filepath.Join(prevDir, "panes", pp.ContentFile), dst) == nil {
						continue
					}
				}
				if err := s.writeCompressed(dst, data); err != nil {
					os.RemoveAll(tmp)
					return nil, err
				}
			}
		}
	}
	if err := writeJSONAtomic(filepath.Join(tmp, "layout.json"), snap); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	return &Staged{store: s, tmpDir: tmp, name: name}, nil
}

func (s *Store) writeCompressed(path string, data []byte) error {
	f, err := os.OpenFile(path+".part", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w, err := s.Codec.NewWriter(f)
	if err != nil {
		f.Close()
		return err
	}
	if _, err := w.Write(data); err != nil {
		w.Close(); f.Close()
		return err
	}
	if err := w.Close(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(path+".part", path)
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path+".part", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(path+".part", path)
}

func (st *Staged) Promote() (string, error) {
	final := filepath.Join(st.store.Dir, st.name)
	if err := os.Rename(st.tmpDir, final); err != nil {
		return "", err
	}
	link := filepath.Join(st.store.Dir, "last")
	tmpLink := link + ".tmp"
	os.Remove(tmpLink)
	if err := os.Symlink(st.name, tmpLink); err != nil {
		return "", err
	}
	return final, os.Rename(tmpLink, link)
}

func (st *Staged) Reject() (string, error) {
	dst := filepath.Join(st.store.Dir, "rejected", st.name)
	os.RemoveAll(dst)
	return dst, os.Rename(st.tmpDir, dst)
}

func (st *Staged) Discard() error { return os.RemoveAll(st.tmpDir) }

// Last returns the snapshot `last` points at. os.ErrNotExist if none.
func (s *Store) Last() (*Snapshot, string, error) {
	target, err := os.Readlink(filepath.Join(s.Dir, "last"))
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Join(s.Dir, target)
	snap, err := s.Load(dir)
	return snap, dir, err
}

func (s *Store) Load(dir string) (*Snapshot, error) {
	b, err := os.ReadFile(filepath.Join(dir, "layout.json"))
	if err != nil {
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	if snap.Schema != SchemaVersion {
		return nil, fmt.Errorf("%s: unsupported schema %d", dir, snap.Schema)
	}
	return &snap, nil
}

// ReadContent decodes a pane's scrollback using the codec named in the snapshot.
func (s *Store) ReadContent(dir string, p Pane) ([]byte, error) {
	if p.ContentFile == "" {
		return nil, os.ErrNotExist
	}
	snap, err := s.Load(dir)
	if err != nil {
		return nil, err
	}
	codec, ok := LookupCodec(snap.ContentsCodec)
	if !ok {
		return nil, fmt.Errorf("unknown codec %q", snap.ContentsCodec)
	}
	f, err := os.Open(filepath.Join(dir, "panes", p.ContentFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := codec.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var sb strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return []byte(sb.String()), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/snapshot/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/snapshot/store.go internal/snapshot/store_test.go
git commit -m "feat(snapshot): directory store with per-pane hardlinked contents, atomic promote/reject, last symlink"
```

---

### Task 7: Guard, event log, freshness marker, retention pruning

**Files:**
- Create: `internal/snapshot/guard.go`, `internal/snapshot/events.go`, `internal/snapshot/prune.go`, `internal/snapshot/guard_test.go`, `internal/snapshot/events_test.go`, `internal/snapshot/prune_test.go`

**Interfaces:**
- Produces: `IsDegenerate(newPanes, lastPanes, minPanes, divisor int) bool`; `Event{Time time.Time; Outcome string; Panes, Windows, Sessions, Clients int; DurationMS int64; File, Detail string}`; `AppendEvent(dir, e) error` (tab-separated line `iso_ts\toutcome\tpanes=N\twindows=N\tsessions=N\tclients=N\tduration_ms=N\tfile\tdetail`); `TailEvents(dir, n) ([]Event, error)`; `TouchFresh(dir) error` (writes `fresh` marker file mtime); `LastGood(dir) (time.Time, bool, error)` = mtime of `fresh`; `Prune(dir, keep, dailyDays, rejectedKeep int, now time.Time) (removed []string, err error)` — keeps the newest `keep` `snap-*` dirs plus the newest per UTC day within `dailyDays`, never removes the `last` target, keeps newest `rejectedKeep` under `rejected/`.

- [ ] **Step 1: Write the failing tests**

`guard_test.go`:
```go
package snapshot

import "testing"

func TestIsDegenerate(t *testing.T) {
	cases := []struct{ n, last int; want bool }{
		{1, 35, true}, {11, 35, true}, {12, 35, false}, {35, 35, false},
		{1, 4, false},  // last not rich
		{0, 5, true}, {5, 5, false},
	}
	for _, c := range cases {
		if got := IsDegenerate(c.n, c.last, 5, 3); got != c.want {
			t.Errorf("IsDegenerate(%d,%d)=%v want %v", c.n, c.last, got, c.want)
		}
	}
}
```

`events_test.go`:
```go
package snapshot

import (
	"testing"
	"time"
)

func TestEventsAppendTailFresh(t *testing.T) {
	dir := t.TempDir()
	if _, ok, _ := LastGood(dir); ok {
		t.Fatal("no marker yet")
	}
	e := Event{Time: time.Date(2026, 8, 22, 1, 2, 3, 0, time.UTC), Outcome: "kept", Panes: 46, Windows: 40,
		Sessions: 6, Clients: 2, DurationMS: 310, File: "snap-x", Detail: ""}
	if err := AppendEvent(dir, e); err != nil {
		t.Fatal(err)
	}
	AppendEvent(dir, Event{Time: e.Time.Add(time.Minute), Outcome: "rejected-degenerate", Panes: 1, Detail: "1 vs 46"})
	got, err := TailEvents(dir, 5)
	if err != nil || len(got) != 2 {
		t.Fatalf("tail = %+v %v", got, err)
	}
	if got[0].Outcome != "kept" || got[0].Panes != 46 || got[0].DurationMS != 310 || got[1].Detail != "1 vs 46" {
		t.Fatalf("parsed %+v", got)
	}
	if err := TouchFresh(dir); err != nil {
		t.Fatal(err)
	}
	if ts, ok, _ := LastGood(dir); !ok || time.Since(ts) > time.Minute {
		t.Fatalf("fresh marker %v %v", ts, ok)
	}
}
```

`prune_test.go`:
```go
package snapshot

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mk(t *testing.T, dir, name string) { t.Helper(); os.MkdirAll(filepath.Join(dir, name), 0o700) }

func TestPruneKeepsRecentDailyAndLast(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	// 4 snapshots today at 10-minute spacing, 3 older days (one each), 1 ancient
	names := []string{"snap-20260822T110000Z", "snap-20260822T111000Z", "snap-20260822T112000Z", "snap-20260822T113000Z",
		"snap-20260821T090000Z", "snap-20260820T090000Z", "snap-20260819T090000Z", "snap-20260601T090000Z"}
	for _, n := range names {
		mk(t, dir, n)
	}
	os.Symlink("snap-20260822T113000Z", filepath.Join(dir, "last"))
	mk(t, dir, "rejected/snap-20260822T100000Z")
	mk(t, dir, "rejected/snap-20260822T100100Z")
	removed, err := Prune(dir, 2, 30, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	exists := func(n string) bool { _, err := os.Stat(filepath.Join(dir, n)); return err == nil }
	for _, keep := range []string{"snap-20260822T113000Z", "snap-20260822T112000Z", // newest 2
		"snap-20260821T090000Z", "snap-20260820T090000Z", "snap-20260819T090000Z", // daily within 30d
		"rejected/snap-20260822T100100Z"} {
		if !exists(keep) {
			t.Errorf("%s should be kept", keep)
		}
	}
	for _, gone := range []string{"snap-20260822T110000Z", "snap-20260822T111000Z", "snap-20260601T090000Z", "rejected/snap-20260822T100000Z"} {
		if exists(gone) {
			t.Errorf("%s should be removed", gone)
		}
	}
	if len(removed) != 4 {
		t.Errorf("removed = %v", removed)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/snapshot/ -run 'TestIsDegenerate|TestEvents|TestPrune' -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Write minimal implementation**

`guard.go`:
```go
package snapshot

// IsDegenerate reports whether a new save collapsed enough, relative to a
// rich last save, to look like an accidental clobber (e.g. a 1-pane save
// right after boot). Only fires when last had >= minPanes.
func IsDegenerate(newPanes, lastPanes, minPanes, divisor int) bool {
	if lastPanes < minPanes {
		return false
	}
	return newPanes*divisor <= lastPanes
}
```

`events.go`:
```go
package snapshot

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Event struct {
	Time                              time.Time
	Outcome                           string
	Panes, Windows, Sessions, Clients int
	DurationMS                        int64
	File, Detail                      string
}

const eventsFile = "events.log"
const freshFile = "fresh"

func AppendEvent(dir string, e Event) error {
	f, err := os.OpenFile(filepath.Join(dir, eventsFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\t%s\tpanes=%d\twindows=%d\tsessions=%d\tclients=%d\tduration_ms=%d\t%s\t%s\n",
		e.Time.UTC().Format(time.RFC3339), e.Outcome, e.Panes, e.Windows, e.Sessions, e.Clients, e.DurationMS, e.File, e.Detail)
	return err
}

func TailEvents(dir string, n int) ([]Event, error) {
	f, err := os.Open(filepath.Join(dir, eventsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var all []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if e, ok := parseEvent(sc.Text()); ok {
			all = append(all, e)
		}
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, sc.Err()
}

func parseEvent(line string) (Event, bool) {
	f := strings.Split(line, "\t")
	if len(f) < 9 {
		return Event{}, false
	}
	ts, err := time.Parse(time.RFC3339, f[0])
	if err != nil {
		return Event{}, false
	}
	kv := func(s string) int { i, _ := strconv.Atoi(s[strings.IndexByte(s, '=')+1:]); return i }
	return Event{Time: ts, Outcome: f[1], Panes: kv(f[2]), Windows: kv(f[3]), Sessions: kv(f[4]),
		Clients: kv(f[5]), DurationMS: int64(kv(f[6])), File: f[7], Detail: f[8]}, true
}

func TouchFresh(dir string) error {
	p := filepath.Join(dir, freshFile)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	now := time.Now()
	return os.Chtimes(p, now, now)
}

func LastGood(dir string) (time.Time, bool, error) {
	fi, err := os.Stat(filepath.Join(dir, freshFile))
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	return fi.ModTime(), true, nil
}
```

`prune.go`:
```go
package snapshot

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func snapTime(name string) (time.Time, bool) {
	t, err := time.Parse(dirTimeFormat, strings.TrimPrefix(name, "snap-"))
	return t, err == nil
}

func listSnaps(dir string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, "snap-*"))
	var out []string
	for _, p := range m {
		if strings.HasSuffix(p, ".tmp") {
			continue
		}
		if _, ok := snapTime(filepath.Base(p)); ok {
			out = append(out, filepath.Base(p))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out))) // newest first (timestamps sort lexically)
	return out
}

// Prune removes snapshot dirs outside the retention policy. Never removes the
// `last` target. Returns the names removed.
func Prune(dir string, keep, dailyDays, rejectedKeep int, now time.Time) ([]string, error) {
	lastTarget, _ := os.Readlink(filepath.Join(dir, "last"))
	keepSet := map[string]bool{lastTarget: true}
	snaps := listSnaps(dir)
	for i, n := range snaps {
		if i < keep {
			keepSet[n] = true
		}
	}
	seenDay := map[string]bool{}
	cutoff := now.AddDate(0, 0, -dailyDays)
	for _, n := range snaps { // newest first → first per day wins
		t, _ := snapTime(n)
		day := t.Format("2006-01-02")
		if t.After(cutoff) && !seenDay[day] {
			seenDay[day] = true
			keepSet[n] = true
		}
	}
	var removed []string
	for _, n := range snaps {
		if !keepSet[n] {
			if err := os.RemoveAll(filepath.Join(dir, n)); err != nil {
				return removed, err
			}
			removed = append(removed, n)
		}
	}
	rej := listSnaps(filepath.Join(dir, "rejected"))
	for i, n := range rej {
		if i >= rejectedKeep {
			if err := os.RemoveAll(filepath.Join(dir, "rejected", n)); err != nil {
				return removed, err
			}
			removed = append(removed, "rejected/"+n)
		}
	}
	return removed, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/snapshot/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/snapshot
git commit -m "feat(snapshot): degenerate guard, tab-separated events log, freshness marker, retention pruning"
```

---

### Task 8: Pane process resolution (allowlist, Claude, argv fallback)

**Files:**
- Create: `internal/procs/resolve.go`, `internal/procs/resolve_test.go`

**Interfaces:**
- Consumes: `Table`, `ClaudeRegistry` (Task 4); `snapshot.Restore` (Task 5).
- Produces: `Resolve(t *Table, reg ClaudeRegistry, panePID int, allowlist []string) snapshot.Restore` — rules, in order: (1) any `claude` comm in the subtree with a registry hit → `{Kind:"claude", ClaudeSession: id}`; (2) any process whose cmdline matches `claude-resume <uuid>` or `--resume <uuid>` → `{Kind:"claude", ClaudeSession: uuid}`; (3) the shallowest non-shell descendant whose comm is in `allowlist` → `{Kind:"argv", Argv: cmdline}`; (4) otherwise `{Kind:"shell"}`. `DefaultAllowlist = []string{"ssh","mosh-client","claude","claude-resume","vi","vim","nvim","emacs","man","less","more","tail","top","htop"}`.

- [ ] **Step 1: Write the failing test**

`resolve_test.go` (uses the Task 4 fixtures; add `300/stat` `300 (bash) S 1 …`, `300/cmdline` `bash\0`, `301/stat` `301 (ssh) S 300 …`, `301/cmdline` `ssh\0-A\0host.example\0`):
```go
package procs

import "testing"

func TestResolve(t *testing.T) {
	tb, _ := Scan("testdata/proc")
	reg := ClaudeRegistry{Dir: "testdata/sessions"}
	r := Resolve(tb, reg, 100, DefaultAllowlist) // bash → claude(registry) → tail
	if r.Kind != "claude" || r.ClaudeSession != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("registry claude: %+v", r)
	}
	r = Resolve(tb, reg, 200, DefaultAllowlist) // python3 claude-resume <uuid> placeholder
	if r.Kind != "claude" || r.ClaudeSession != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("argv claude: %+v", r)
	}
	r = Resolve(tb, reg, 300, DefaultAllowlist) // bash → ssh
	if r.Kind != "argv" || len(r.Argv) != 3 || r.Argv[0] != "ssh" {
		t.Fatalf("ssh argv: %+v", r)
	}
	r = Resolve(tb, reg, 300, nil) // nothing allowed → shell
	if r.Kind != "shell" {
		t.Fatalf("shell: %+v", r)
	}
	if r := Resolve(tb, reg, 4242, DefaultAllowlist); r.Kind != "shell" {
		t.Fatalf("unknown pid: %+v", r)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/procs/ -run TestResolve -v`
Expected: FAIL — `undefined: Resolve`.

- [ ] **Step 3: Write minimal implementation**

`resolve.go`:
```go
package procs

import (
	"regexp"
	"strings"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

var DefaultAllowlist = []string{"ssh", "mosh-client", "claude", "claude-resume", "vi", "vim", "nvim", "emacs", "man", "less", "more", "tail", "top", "htop"}

var resumeRe = regexp.MustCompile(`(?:claude-resume|--resume)\s+([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

var shells = map[string]bool{"bash": true, "zsh": true, "sh": true, "fish": true, "dash": true}

// Resolve decides how a pane should be restored from its process subtree.
func Resolve(t *Table, reg ClaudeRegistry, panePID int, allowlist []string) snapshot.Restore {
	if _, ok := t.Get(panePID); !ok {
		return snapshot.Restore{Kind: "shell"}
	}
	pids := t.Subtree(panePID)
	for _, pid := range pids {
		if p, _ := t.Get(pid); p.Comm == "claude" {
			if sid, ok := reg.SessionFor(p); ok {
				return snapshot.Restore{Kind: "claude", ClaudeSession: sid}
			}
		}
	}
	for _, pid := range pids {
		p, _ := t.Get(pid)
		if m := resumeRe.FindStringSubmatch(strings.Join(p.Cmdline, " ")); m != nil {
			return snapshot.Restore{Kind: "claude", ClaudeSession: m[1]}
		}
	}
	allowed := map[string]bool{}
	for _, a := range allowlist {
		allowed[a] = true
	}
	for _, pid := range pids[1:] { // skip the pane's own shell
		p, _ := t.Get(pid)
		if shells[p.Comm] {
			continue
		}
		if allowed[p.Comm] && len(p.Cmdline) > 0 {
			return snapshot.Restore{Kind: "argv", Argv: append([]string(nil), p.Cmdline...)}
		}
	}
	return snapshot.Restore{Kind: "shell"}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/procs/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/procs
git commit -m "feat(procs): pane restore resolution — registry claude, argv uuid fallback, allowlist, shell"
```

---

### Task 9: Collector — build snapshot + contents from tmux (fake transport)

**Files:**
- Create: `internal/collect/collect.go`, `internal/collect/collect_test.go`, `internal/collect/testdata/fleet.transcript` (not a transcript; a Go map literal inside the test is simpler — use the Fake)

**Interfaces:**
- Consumes: `tmuxctl.Transport`, `procs.Table/ClaudeRegistry/Resolve`, `snapshot.*`.
- Produces: `Collector{T tmuxctl.Transport; Procs *procs.Table; Reg procs.ClaudeRegistry; Allowlist []string; Host string; Now func() time.Time}`; `(*Collector).Collect(ctx) (*snapshot.Snapshot, map[string][]byte, error)`.
- Exact tmux commands issued (later tasks and fakes depend on these strings):
  - `SessCmd = "list-sessions -F \"#{session_name}\t#{session_grouped}\t#{session_attached}\""`
  - `WinCmd = "list-windows -a -F \"#{session_name}\t#{window_index}\t#{window_name}\t#{window_active}\t#{window_flags}\t#{window_layout}\t#{automatic-rename}\""`  (tmux ≥3.1 exposes window options via `#{automatic-rename}`)
  - `PaneCmd = "list-panes -a -F \"#{session_name}\t#{window_index}\t#{pane_index}\t#{pane_id}\t#{pane_active}\t#{pane_pid}\t#{pane_current_path}\t#{pane_title}\t#{history_size}\""`
  - `ServerCmd = "display-message -p \"#{start_time}\t#{version}\t#{client_session}\""`
  - per pane: `fmt.Sprintf("capture-pane -epJ -S -%d -t %s", historySize, paneID)`
- Panes in grouped sessions (`session_grouped` = 1) are skipped. `Stats.DurationMS` measured around Collect. Sessions/windows/panes sorted by name/index.

- [ ] **Step 1: Write the failing test**

`collect_test.go`:
```go
package collect

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mithro/go-tmux-saver/internal/procs"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

func fakeServer() *tmuxctl.Fake {
	return &tmuxctl.Fake{Replies: map[string][]string{
		SessCmd:   {"default\t0\t1", "default-1\t1\t1", "net\t0\t0"},
		WinCmd:    {"default\t0\th\t1\t*\tbfbf,80x24,0,0,0\ton", "default-1\t0\th\t1\t*\tx\ton", "net\t2\tswcfg\t1\t*\tdead,80x24,0,0{40x24,0,0,1,39x24,41,0,2}\toff"},
		PaneCmd:   {"default\t0\t0\t%0\t1\t100\t/home/tim\ttim@ten64: ~\t3", "default-1\t0\t0\t%0\t1\t100\t/home/tim\tx\t3",
			"net\t2\t0\t%1\t1\t300\t/home/tim/net\t✳ switch config\t2", "net\t2\t1\t%2\t0\t200\t/home/tim\tten64\t0"},
		ServerCmd: {"1787201600\tnext-3.8\tdefault"},
		"capture-pane -epJ -S -3 -t %0": {"a", "b", "c"},
		"capture-pane -epJ -S -2 -t %1": {"x", "y"},
		"capture-pane -epJ -S -0 -t %2": {""},
	}}
}

func TestCollectBuildsSnapshot(t *testing.T) {
	f := fakeServer()
	tb, _ := procs.Scan("../procs/testdata/proc")
	c := &Collector{T: f, Procs: tb, Reg: procs.ClaudeRegistry{Dir: "../procs/testdata/sessions"},
		Allowlist: procs.DefaultAllowlist, Host: "ten64", Now: func() time.Time { return time.Unix(100, 0).UTC() }}
	snap, contents, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 2 { // default-1 is a grouped clone → skipped
		t.Fatalf("sessions = %+v", snap.Sessions)
	}
	if snap.TmuxVersion != "next-3.8" || snap.ServerStart != 1787201600 || snap.Client.Session != "default" || snap.Host != "ten64" {
		t.Fatalf("server fields %+v", snap)
	}
	net := snap.Sessions[1]
	if net.Name != "net" || net.Windows[0].Index != 2 || net.Windows[0].Layout != "dead,80x24,0,0{40x24,0,0,1,39x24,41,0,2}" || net.Windows[0].AutomaticRename {
		t.Fatalf("net window %+v", net.Windows[0])
	}
	p0 := net.Windows[0].Panes[0]
	if p0.Restore.Kind != "argv" || p0.Restore.Argv[0] != "ssh" || p0.Title != "✳ switch config" || !p0.Active {
		t.Fatalf("pane0 %+v", p0)
	}
	if net.Windows[0].Panes[1].Restore.Kind != "claude" {
		t.Fatalf("pane1 should be claude placeholder: %+v", net.Windows[0].Panes[1])
	}
	if string(contents["net_2_0"]) != "x\ny\n" || string(contents["default_0_0"]) != "a\nb\nc\n" {
		t.Fatalf("contents %q", contents)
	}
	if _, ok := contents["default-1_0_0"]; ok {
		t.Fatal("grouped clone must not be captured")
	}
	for _, call := range f.Calls {
		if strings.Contains(call, "-t %0") && strings.Count(strings.Join(f.Calls, "\n"), "-t %0") > 1 {
			t.Fatal("pane %0 captured more than once")
		}
	}
	p, w := snap.CountPanes()
	if p != 3 || w != 2 || snap.Stats.Panes != 3 {
		t.Fatalf("counts %d %d %+v", p, w, snap.Stats)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/collect/ -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Write minimal implementation**

`collect.go`:
```go
// Package collect builds a snapshot from a live tmux server.
package collect

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mithro/go-tmux-saver/internal/procs"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

const (
	SessCmd   = "list-sessions -F \"#{session_name}\t#{session_grouped}\t#{session_attached}\""
	WinCmd    = "list-windows -a -F \"#{session_name}\t#{window_index}\t#{window_name}\t#{window_active}\t#{window_flags}\t#{window_layout}\t#{automatic-rename}\""
	PaneCmd   = "list-panes -a -F \"#{session_name}\t#{window_index}\t#{pane_index}\t#{pane_id}\t#{pane_active}\t#{pane_pid}\t#{pane_current_path}\t#{pane_title}\t#{history_size}\""
	ServerCmd = "display-message -p \"#{start_time}\t#{version}\t#{client_session}\""
)

type Collector struct {
	T         tmuxctl.Transport
	Procs     *procs.Table
	Reg       procs.ClaudeRegistry
	Allowlist []string
	Host      string
	Now       func() time.Time
}

func (c *Collector) Collect(ctx context.Context) (*snapshot.Snapshot, map[string][]byte, error) {
	start := time.Now()
	now := c.Now
	if now == nil {
		now = time.Now
	}
	snap := &snapshot.Snapshot{Schema: snapshot.SchemaVersion, Host: c.Host, TakenAt: now().UTC()}

	if lines, err := c.T.Run(ctx, ServerCmd); err == nil && len(lines) == 1 {
		f := strings.Split(lines[0], "\t")
		if len(f) == 3 {
			snap.ServerStart, _ = strconv.ParseInt(f[0], 10, 64)
			snap.TmuxVersion = f[1]
			snap.Client.Session = f[2]
		}
	} else if err != nil {
		return nil, nil, fmt.Errorf("server info: %w", err)
	}

	sessLines, err := c.T.Run(ctx, SessCmd)
	if err != nil {
		return nil, nil, fmt.Errorf("list-sessions: %w", err)
	}
	sessions := map[string]*snapshot.Session{}
	var order []string
	for _, l := range sessLines {
		f := strings.Split(l, "\t")
		if len(f) < 2 || f[1] == "1" {
			continue // grouped clone
		}
		sessions[f[0]] = &snapshot.Session{Name: f[0]}
		order = append(order, f[0])
	}
	sort.Strings(order)

	winLines, err := c.T.Run(ctx, WinCmd)
	if err != nil {
		return nil, nil, fmt.Errorf("list-windows: %w", err)
	}
	windows := map[string]*snapshot.Window{} // key session\tindex
	for _, l := range winLines {
		f := strings.Split(l, "\t")
		if len(f) < 7 {
			continue
		}
		se, ok := sessions[f[0]]
		if !ok {
			continue
		}
		idx, _ := strconv.Atoi(f[1])
		w := snapshot.Window{Index: idx, Name: f[2], Active: f[3] == "1", Flags: f[4], Layout: f[5], AutomaticRename: f[6] == "on"}
		if w.Active {
			se.ActiveWindow = idx
		}
		se.Windows = append(se.Windows, w)
		windows[f[0]+"\t"+f[1]] = &se.Windows[len(se.Windows)-1]
	}

	paneLines, err := c.T.Run(ctx, PaneCmd)
	if err != nil {
		return nil, nil, fmt.Errorf("list-panes: %w", err)
	}
	type capture struct {
		key, id string
		hist    int
	}
	var caps []capture
	for _, l := range paneLines {
		f := strings.Split(l, "\t")
		if len(f) < 9 {
			continue
		}
		w, ok := windows[f[0]+"\t"+f[1]]
		if !ok {
			continue
		}
		idx, _ := strconv.Atoi(f[2])
		pid, _ := strconv.Atoi(f[5])
		hist, _ := strconv.Atoi(f[8])
		p := snapshot.Pane{Index: idx, ID: f[3], Active: f[4] == "1", Cwd: f[6], Title: f[7], HistoryLines: hist,
			Restore: procs.Resolve(c.Procs, c.Reg, pid, c.Allowlist)}
		w.Panes = append(w.Panes, p)
		caps = append(caps, capture{snapshot.PaneKey(f[0], w.Index, idx), f[3], hist})
	}

	contents := map[string][]byte{}
	for _, cp := range caps {
		lines, err := c.T.Run(ctx, fmt.Sprintf("capture-pane -epJ -S -%d -t %s", cp.hist, cp.id))
		if err != nil {
			return nil, nil, fmt.Errorf("capture %s: %w", cp.id, err)
		}
		contents[cp.key] = []byte(strings.Join(lines, "\n") + "\n")
	}

	for _, name := range order {
		se := sessions[name]
		sort.Slice(se.Windows, func(i, j int) bool { return se.Windows[i].Index < se.Windows[j].Index })
		for i := range se.Windows {
			ps := se.Windows[i].Panes
			sort.Slice(ps, func(a, b int) bool { return ps[a].Index < ps[b].Index })
		}
		snap.Sessions = append(snap.Sessions, *se)
	}
	p, w := snap.CountPanes()
	snap.Stats = snapshot.Stats{Panes: p, Windows: w, Sessions: len(snap.Sessions), DurationMS: time.Since(start).Milliseconds()}
	return snap, contents, nil
}
```

Note: the window-map-pointer approach (`&se.Windows[len-1]`) is invalidated by later appends to `se.Windows`; implement instead with an index map `winIdx[key] = position` and take `&se.Windows[pos]` *after* all windows are appended (two passes), or build `[]*snapshot.Window` and flatten at the end. Tests will catch this; keep the two-pass version.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/collect/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/collect
git commit -m "feat(collect): build snapshot and per-pane contents over one control-mode connection"
```

---

### Task 10: `Unchanged` comparison with the title rule

**Files:**
- Create: `internal/collect/unchanged.go`, `internal/collect/unchanged_test.go`

**Interfaces:**
- Produces: `Unchanged(a, b *snapshot.Snapshot) bool` — true when sessions/windows/panes structure, names, layouts, flags, cwds, restore commands and active markers are equal; ignores `TakenAt`, `Stats`, `HistoryLines`, `ContentSHA256/ContentFile`, and pane titles **unless** the title begins with `✳` (Claude summary titles are compared).

- [ ] **Step 1: Write the failing test**

```go
package collect

import (
	"testing"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

func base() *snapshot.Snapshot {
	return &snapshot.Snapshot{Sessions: []snapshot.Session{{Name: "s", Windows: []snapshot.Window{{Index: 0, Name: "w", Layout: "L",
		Panes: []snapshot.Pane{{Index: 0, Cwd: "/a", Title: "tim@ten64: ~", HistoryLines: 5, Restore: snapshot.Restore{Kind: "shell"}}}}}}}}
}

func TestUnchanged(t *testing.T) {
	a, b := base(), base()
	b.Stats.Panes = 99
	b.Sessions[0].Windows[0].Panes[0].HistoryLines = 500
	b.Sessions[0].Windows[0].Panes[0].ContentSHA256 = "zzz"
	b.Sessions[0].Windows[0].Panes[0].Title = "tim@ten64: ~/elsewhere" // shell title churn
	if !Unchanged(a, b) {
		t.Fatal("metadata/shell-title changes must be ignored")
	}
	c := base()
	c.Sessions[0].Windows[0].Panes[0].Cwd = "/b"
	if Unchanged(a, c) {
		t.Fatal("cwd change is a change")
	}
	d, e := base(), base()
	d.Sessions[0].Windows[0].Panes[0].Title = "✳ fixing dns"
	e.Sessions[0].Windows[0].Panes[0].Title = "✳ fixing dhcp"
	if Unchanged(d, e) {
		t.Fatal("claude ✳ titles are state")
	}
	f := base()
	f.Sessions[0].Windows = append(f.Sessions[0].Windows, snapshot.Window{Index: 1, Name: "x"})
	if Unchanged(a, f) {
		t.Fatal("extra window is a change")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/collect/ -run TestUnchanged -v` → FAIL undefined.

- [ ] **Step 3: Write minimal implementation**

```go
package collect

import (
	"strings"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

func Unchanged(a, b *snapshot.Snapshot) bool {
	if len(a.Sessions) != len(b.Sessions) {
		return false
	}
	for i := range a.Sessions {
		sa, sb := a.Sessions[i], b.Sessions[i]
		if sa.Name != sb.Name || sa.ActiveWindow != sb.ActiveWindow || len(sa.Windows) != len(sb.Windows) {
			return false
		}
		for j := range sa.Windows {
			wa, wb := sa.Windows[j], sb.Windows[j]
			if wa.Index != wb.Index || wa.Name != wb.Name || wa.Layout != wb.Layout || wa.Active != wb.Active ||
				wa.Flags != wb.Flags || wa.AutomaticRename != wb.AutomaticRename || len(wa.Panes) != len(wb.Panes) {
				return false
			}
			for k := range wa.Panes {
				pa, pb := wa.Panes[k], wb.Panes[k]
				if pa.Index != pb.Index || pa.Cwd != pb.Cwd || pa.Active != pb.Active || !restoreEqual(pa.Restore, pb.Restore) {
					return false
				}
				if (strings.HasPrefix(pa.Title, "✳") || strings.HasPrefix(pb.Title, "✳")) && pa.Title != pb.Title {
					return false
				}
			}
		}
	}
	return true
}

func restoreEqual(x, y snapshot.Restore) bool {
	if x.Kind != y.Kind || x.ClaudeSession != y.ClaudeSession || len(x.Argv) != len(y.Argv) {
		return false
	}
	for i := range x.Argv {
		if x.Argv[i] != y.Argv[i] {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run tests to verify they pass** → `go test ./internal/collect/ -v` PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/unchanged.go internal/collect/unchanged_test.go
git commit -m "feat(collect): structural Unchanged comparison ignoring shell titles but honouring Claude ✳ titles"
```

---

### Task 11: Config (defaults, load, validate)

**Files:**
- Create: `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces:
```go
type Config struct {
	Socket           string   `json:"socket"`            // "main"
	SeedSession      string   `json:"seed_session"`      // "default"
	SeedWindow       string   `json:"seed_window"`       // "h"
	IntervalMinutes  int      `json:"interval_minutes"`  // 10
	WatchStaleFactor int      `json:"watch_stale_factor"`// 3
	Allowlist        []string `json:"allowlist"`
	Guard            struct{ MinPanes int `json:"min_panes"`; Divisor int `json:"divisor"` } `json:"guard"`
	Contents         struct{ Enabled bool `json:"enabled"`; Codec string `json:"codec"` } `json:"contents"`
	Retention        struct{ Keep int `json:"keep"`; DailyDays int `json:"daily_days"`; Rejected int `json:"rejected"` } `json:"retention"`
	MailTo           string   `json:"mail_to"`           // $USER
	ClaudeResumePath string   `json:"claude_resume_path"`// ~/bin/claude-resume
	DataDir          string   `json:"-"`                 // derived: $XDG_DATA_HOME/go-tmux-saver
}
func Default() Config
func Path() string                     // $XDG_CONFIG_HOME/go-tmux-saver/config.json
func Load(path string) (Config, error) // defaults overlaid by file; missing file → defaults
func (c Config) Validate() error       // interval ≥1, divisor ≥2, codec registered, allowlist non-empty
func DataDir() string                  // $XDG_DATA_HOME or ~/.local/share + /go-tmux-saver
```

- [ ] **Step 1: Write the failing test**

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsAndLoad(t *testing.T) {
	d := Default()
	if d.Socket != "main" || d.IntervalMinutes != 10 || d.Guard.MinPanes != 5 || d.Guard.Divisor != 3 ||
		d.Contents.Codec != "gzip" || !d.Contents.Enabled || d.Retention.Keep != 50 || d.SeedSession != "default" || d.SeedWindow != "h" {
		t.Fatalf("defaults %+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte(`{"interval_minutes": 5, "guard": {"divisor": 4}, "contents": {"codec": "nope"}}`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.IntervalMinutes != 5 || c.Guard.Divisor != 4 || c.Guard.MinPanes != 5 || c.Socket != "main" {
		t.Fatalf("overlay %+v", c)
	}
	if err := c.Validate(); err == nil {
		t.Fatal("unknown codec must fail validation")
	}
	if c, err := Load(filepath.Join(t.TempDir(), "missing.json")); err != nil || c.Socket != "main" {
		t.Fatalf("missing file should yield defaults: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", "/x")
	if DataDir() != "/x/go-tmux-saver" {
		t.Fatal(DataDir())
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/config/ -v` → FAIL undefined.

- [ ] **Step 3: Implement** `config.go` with `Default()` filling the values above (`Allowlist: procs.DefaultAllowlist`, `MailTo: os.Getenv("USER")`, `ClaudeResumePath: "~/bin/claude-resume"`), `Load` = `Default()` then `json.Unmarshal` over it (returns defaults on `os.IsNotExist`), `Validate` checks: `IntervalMinutes >= 1`, `Guard.Divisor >= 2`, `Guard.MinPanes >= 1`, `snapshot.LookupCodec(Contents.Codec)` ok, `len(Allowlist) > 0`, `Retention.Keep >= 1`; `Path()`/`DataDir()` from `XDG_CONFIG_HOME`/`XDG_DATA_HOME` falling back to `~/.config`, `~/.local/share`.

- [ ] **Step 4: Run** → PASS.

- [ ] **Step 5: Commit** `git commit -am "feat(config): defaults, XDG paths, JSON overlay load, validation"` (after `git add internal/config`).

---

### Task 12: `save` command (outcomes, guard, events, prune, display-message)

**Files:**
- Create: `internal/cli/save.go`, `internal/cli/save_test.go`, `internal/cli/common.go`

**Interfaces:**
- Consumes: everything above.
- Produces: `cli` subcommand `save [--auto] [--socket S] [--data-dir D] [--config P] [--no-display]`; exported for tests: `func RunSave(ctx context.Context, deps SaveDeps) (Outcome, error)` where
```go
type SaveDeps struct { T tmuxctl.Transport; Store *snapshot.Store; Procs *procs.Table; Reg procs.ClaudeRegistry; Cfg config.Config; Host string; Clients int; Display func(msg string) }
type Outcome struct { Kind string /* kept|unchanged|rejected-degenerate|skipped|error */; Dir string; Panes, LastPanes int; Duration time.Duration }
```
- Behaviour: `Collect` → if `Last()` exists and `Unchanged` → `Discard`, `TouchFresh`, event `unchanged`; else if `IsDegenerate(new, last, cfg)` → `Reject`, event `rejected-degenerate` with detail `"<new> vs <last>"`; else `Promote`, `TouchFresh`, event `kept`, then `Prune`. On any collection/store error → event `error` with detail. `common.go` holds `openTransport(cfg) (tmuxctl.Transport, error)` which returns a sentinel `ErrNoServer` when `Dial` fails because no server is listening (tmux exits with "no server running" / "error connecting") → the CLI maps that to `skipped` exit 0 (only in `--auto`; manual save reports the error, exit 1). `Display` runs `display-message "<summary>"` via the transport unless `--no-display`.

- [ ] **Step 1: Write the failing test**

`save_test.go` (uses the Task 9 fake and Task 4 fixtures):
```go
package cli

import (
	"context"
	"testing"
	"time"

	"github.com/mithro/go-tmux-saver/internal/collect"
	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/procs"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

func saveFake() *tmuxctl.Fake {
	return &tmuxctl.Fake{Replies: map[string][]string{
		collect.SessCmd:   {"default\t0\t1"},
		collect.WinCmd:    {"default\t0\tw\t1\t*\tL\ton"},
		collect.PaneCmd:   {"default\t0\t0\t%0\t1\t100\t/home/tim\tt\t1", "default\t0\t1\t%1\t0\t300\t/home/tim\tt\t1"},
		collect.ServerCmd: {"1\tnext-3.8\tdefault"},
		"capture-pane -epJ -S -1 -t %0": {"a"}, "capture-pane -epJ -S -1 -t %1": {"b"},
	}, Default: []string{}}
}

func deps(t *testing.T, f *tmuxctl.Fake) SaveDeps {
	gz, _ := snapshot.LookupCodec("gzip")
	st := &snapshot.Store{Dir: t.TempDir(), Codec: gz}
	st.EnsureDir()
	tb, _ := procs.Scan("../procs/testdata/proc")
	cfg := config.Default()
	cfg.Guard.MinPanes = 2
	return SaveDeps{T: f, Store: st, Procs: tb, Reg: procs.ClaudeRegistry{Dir: "../procs/testdata/sessions"}, Cfg: cfg, Host: "h", Display: func(string) {}}
}

func TestSaveOutcomes(t *testing.T) {
	d := deps(t, saveFake())
	ctx := context.Background()
	o, err := RunSave(ctx, d)
	if err != nil || o.Kind != "kept" || o.Panes != 2 {
		t.Fatalf("first save %+v %v", o, err)
	}
	o, _ = RunSave(ctx, d)
	if o.Kind != "unchanged" {
		t.Fatalf("second identical save should be unchanged, got %+v", o)
	}
	// degenerate: server now shows 0 panes
	d.T = &tmuxctl.Fake{Replies: map[string][]string{collect.SessCmd: {"default\t0\t1"}, collect.WinCmd: {}, collect.PaneCmd: {},
		collect.ServerCmd: {"1\tnext-3.8\tdefault"}}, Default: []string{}}
	o, _ = RunSave(ctx, d)
	if o.Kind != "rejected-degenerate" || o.LastPanes != 2 {
		t.Fatalf("degenerate %+v", o)
	}
	ev, _ := snapshot.TailEvents(d.Store.Dir, 10)
	if len(ev) != 3 || ev[0].Outcome != "kept" || ev[1].Outcome != "unchanged" || ev[2].Outcome != "rejected-degenerate" || ev[2].Detail != "0 vs 2" {
		t.Fatalf("events %+v", ev)
	}
	if _, ok, _ := snapshot.LastGood(d.Store.Dir); !ok {
		t.Fatal("fresh marker expected")
	}
	if len(d.T.(*tmuxctl.Fake).Calls) == 0 {
		t.Fatal("expected calls")
	}
	_ = time.Second
}
```

- [ ] **Step 2: Run** `go test ./internal/cli/ -run TestSaveOutcomes -v` → FAIL.

- [ ] **Step 3: Implement** `save.go`:
```go
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mithro/go-tmux-saver/internal/collect"
	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/procs"
	"github.com/mithro/go-tmux-saver/internal/snapshot"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

type SaveDeps struct {
	T       tmuxctl.Transport
	Store   *snapshot.Store
	Procs   *procs.Table
	Reg     procs.ClaudeRegistry
	Cfg     config.Config
	Host    string
	Clients int
	Display func(msg string)
}

type Outcome struct {
	Kind            string
	Dir             string
	Panes, LastPanes int
	Duration        time.Duration
}

func RunSave(ctx context.Context, d SaveDeps) (Outcome, error) {
	start := time.Now()
	logEv := func(kind string, snap *snapshot.Snapshot, file, detail string) {
		e := snapshot.Event{Time: time.Now(), Outcome: kind, Clients: d.Clients, DurationMS: time.Since(start).Milliseconds(), File: file, Detail: detail}
		if snap != nil {
			e.Panes, e.Windows = snap.CountPanes()
			e.Sessions = len(snap.Sessions)
		}
		snapshot.AppendEvent(d.Store.Dir, e)
	}
	c := &collect.Collector{T: d.T, Procs: d.Procs, Reg: d.Reg, Allowlist: d.Cfg.Allowlist, Host: d.Host}
	snap, contents, err := c.Collect(ctx)
	if err != nil {
		logEv("error", nil, "", err.Error())
		return Outcome{Kind: "error"}, err
	}
	if !d.Cfg.Contents.Enabled {
		contents = map[string][]byte{}
	}
	newPanes, _ := snap.CountPanes()
	last, _, lerr := d.Store.Last()
	lastPanes := 0
	if lerr == nil {
		lastPanes, _ = last.CountPanes()
		if collect.Unchanged(last, snap) {
			snapshot.TouchFresh(d.Store.Dir)
			logEv("unchanged", snap, "", "")
			d.Display(fmt.Sprintf("unchanged (%d panes)", newPanes))
			return Outcome{Kind: "unchanged", Panes: newPanes, LastPanes: lastPanes, Duration: time.Since(start)}, nil
		}
	}
	stg, err := d.Store.Stage(snap, contents)
	if err != nil {
		logEv("error", snap, "", err.Error())
		return Outcome{Kind: "error"}, err
	}
	if lerr == nil && snapshot.IsDegenerate(newPanes, lastPanes, d.Cfg.Guard.MinPanes, d.Cfg.Guard.Divisor) {
		dir, _ := stg.Reject()
		detail := fmt.Sprintf("%d vs %d", newPanes, lastPanes)
		logEv("rejected-degenerate", snap, filepathBase(dir), detail)
		d.Display("rejected: degenerate (" + detail + ")")
		return Outcome{Kind: "rejected-degenerate", Dir: dir, Panes: newPanes, LastPanes: lastPanes, Duration: time.Since(start)}, nil
	}
	dir, err := stg.Promote()
	if err != nil {
		stg.Discard()
		logEv("error", snap, "", err.Error())
		return Outcome{Kind: "error"}, err
	}
	snapshot.TouchFresh(d.Store.Dir)
	logEv("kept", snap, filepathBase(dir), "")
	snapshot.Prune(d.Store.Dir, d.Cfg.Retention.Keep, d.Cfg.Retention.DailyDays, d.Cfg.Retention.Rejected, time.Now())
	d.Display(fmt.Sprintf("saved %d panes in %.1fs", newPanes, time.Since(start).Seconds()))
	return Outcome{Kind: "kept", Dir: dir, Panes: newPanes, LastPanes: lastPanes, Duration: time.Since(start)}, nil
}

func filepathBase(p string) string { return filepath.Base(p) }

func init() {
	register(command{"save", "snapshot the running tmux server", func(args []string, stdout, stderr io.Writer) int {
		fs := flag.NewFlagSet("save", flag.ContinueOnError)
		auto := fs.Bool("auto", false, "timer mode: no-server is 'skipped' and exits 0")
		noDisplay := fs.Bool("no-display", false, "do not display-message a summary in tmux")
		cfgPath := fs.String("config", config.Path(), "config file")
		fs.SetOutput(stderr)
		if err := fs.Parse(args); err != nil {
			return 2
		}
		cfg, err := config.Load(*cfgPath)
		if err == nil {
			err = cfg.Validate()
		}
		if err != nil {
			fmt.Fprintln(stderr, "config:", err)
			return 2
		}
		codec, _ := snapshot.LookupCodec(cfg.Contents.Codec)
		store := &snapshot.Store{Dir: config.DataDir(), Codec: codec}
		if err := store.EnsureDir(); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		tr, err := openTransport(ctx, cfg)
		if err != nil {
			if *auto && isNoServer(err) {
				snapshot.AppendEvent(store.Dir, snapshot.Event{Time: time.Now(), Outcome: "skipped", Detail: "no server"})
				fmt.Fprintln(stdout, "skipped: no tmux server")
				return 0
			}
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer tr.Close()
		tb, err := procs.Scan("/proc")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		host, _ := os.Hostname()
		home, _ := os.UserHomeDir()
		d := SaveDeps{T: tr, Store: store, Procs: tb, Reg: procs.ClaudeRegistry{Dir: filepath.Join(home, ".claude", "sessions")},
			Cfg: cfg, Host: host, Clients: countClients(ctx, tr), Display: func(string) {}}
		if !*noDisplay {
			d.Display = func(m string) { tr.Run(ctx, fmt.Sprintf("display-message %q", "go-tmux-saver: "+m)) }
		}
		o, err := RunSave(ctx, d)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s panes=%d last=%d %s\n", o.Kind, o.Panes, o.LastPanes, o.Duration.Round(time.Millisecond))
		return 0
	}})
}
```

`common.go`:
```go
package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/mithro/go-tmux-saver/internal/config"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

var ErrNoServer = errors.New("no tmux server running")

func openTransport(ctx context.Context, cfg config.Config) (tmuxctl.Transport, error) {
	c, err := tmuxctl.Dial(ctx, cfg.Socket, cfg.SeedSession)
	if err != nil {
		if strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "error connecting") ||
			strings.Contains(err.Error(), "control connection closed") {
			return nil, ErrNoServer
		}
		return nil, err
	}
	return c, nil
}

func isNoServer(err error) bool { return errors.Is(err, ErrNoServer) }

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
```

Note: `Dial` swallows stderr; to make "no server running" detectable, change `Dial` (Task 3) to capture stderr into a buffer and include it in the error when the initial attach fails. Add a test in Task 3's file `TestDialNoServer` asserting the error message contains "no server running" when dialing socket `gts-nonexistent-<pid>`.

- [ ] **Step 4: Run** `go test ./internal/cli/ ./internal/tmuxctl/ -v` → PASS (including the new `TestDialNoServer`).

- [ ] **Step 5: Commit**

```bash
git add internal/cli internal/tmuxctl
git commit -m "feat(cli): save command — guard, unchanged dedup, events, prune, --auto skipped semantics"
```

---

### Task 13: Live state, seed-only detection, restore planner (pure)

**Files:**
- Create: `internal/restore/live.go`, `internal/restore/plan.go`, `internal/restore/plan_test.go`

**Interfaces:**
- Produces: `LiveWindow{Index int; Name string}`; `LiveState{Sessions map[string][]LiveWindow; Clients int}`; `QueryLive(ctx, t) (LiveState, error)` using `list-windows -a -F "#{session_name}\t#{window_index}\t#{window_name}\t#{session_grouped}"` (grouped clones excluded); `IsSeedOnly(l, seedSession, seedWindow) bool` = exactly one session named seedSession with exactly one window named seedWindow; `Options{ClaudeResumePath string; Contents bool; SeedSession, SeedWindow string}`; `Action{Kind string; Args []string; Note string}` with `Kind` ∈ `{"tmux","note","contents"}` — `tmux` = one command line (`Args[0]`), `contents` = `Args = [paneTarget, paneKey]` (applier cats the file), `note` = log line; `Plan{Actions []Action; Created, Relocated, Skipped int}`; `BuildPlan(live LiveState, snap *snapshot.Snapshot, o Options) Plan`.
- Planner rules (exactly the spec's merge semantics): for each saved session: if absent → `new-session -d -s <name> -n <win0name> -c <cwd0>` then windows; if present → for each saved window: index free → `new-window -d -t <sess>:<idx> -n <name> -c <cwd0>`; occupied by same name → `note skipped`; occupied by different name → `new-window -d -t <sess>: -n <name> -c <cwd0>` (next free) and `note relocated`. For every created window: for panes 1..n `split-window -d -t <target> -c <cwd>`; then `select-layout -t <target> "<layout>"`; for each pane, optional `contents` action, then the restore keystrokes: `send-keys -t <pane-target> "<argv shell-quoted>" Enter` (kind `argv`), `send-keys -t <pane-target> "<ClaudeResumePath> <uuid>" Enter` (kind `claude`), nothing for `shell`; `rename-window` only for *created* windows when `AutomaticRename` is off (`set-window-option -t <target> automatic-rename off` then name already given); finally `select-window -t <sess>:<ActiveWindow>` for created sessions and `select-pane` for the active pane of each created window. Pane targets use `<sess>:<idx>.<pane>`. Quoting helper `shellQuote(argv []string) string` (single-quote each arg, `'\''` inside).

- [ ] **Step 1: Write the failing test**

`plan_test.go`:
```go
package restore

import (
	"strings"
	"testing"

	"github.com/mithro/go-tmux-saver/internal/snapshot"
)

func snapNet() *snapshot.Snapshot {
	return &snapshot.Snapshot{Sessions: []snapshot.Session{
		{Name: "default", ActiveWindow: 1, Windows: []snapshot.Window{
			{Index: 0, Name: "h", Layout: "L0", Panes: []snapshot.Pane{{Index: 0, Cwd: "/home/tim", Restore: snapshot.Restore{Kind: "shell"}}}},
			{Index: 1, Name: "rcfiles", Layout: "L1", AutomaticRename: false, Panes: []snapshot.Pane{{Index: 0, Cwd: "/home/tim/rcfiles", Active: true, Restore: snapshot.Restore{Kind: "claude", ClaudeSession: "abc"}}}},
		}},
		{Name: "net", ActiveWindow: 0, Windows: []snapshot.Window{
			{Index: 0, Name: "swcfg", Layout: "L2", Panes: []snapshot.Pane{
				{Index: 0, Cwd: "/home/tim/net", Active: true, Restore: snapshot.Restore{Kind: "argv", Argv: []string{"ssh", "sw it's"}}},
				{Index: 1, Cwd: "/nonexistent/dir", Restore: snapshot.Restore{Kind: "shell"}}}}}},
	}}
}

func TestIsSeedOnly(t *testing.T) {
	seed := LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}}}}
	if !IsSeedOnly(seed, "default", "h") {
		t.Fatal("seed should be seed-only")
	}
	if IsSeedOnly(LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}, {1, "x"}}}}, "default", "h") {
		t.Fatal("extra window is not seed-only")
	}
	if IsSeedOnly(LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}}, "net": {}}}, "default", "h") {
		t.Fatal("extra session is not seed-only")
	}
}

func TestPlanOnSeedServer(t *testing.T) {
	live := LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}}}}
	p := BuildPlan(live, snapNet(), Options{ClaudeResumePath: "/home/tim/bin/claude-resume", Contents: true, SeedSession: "default", SeedWindow: "h"})
	cmds := strings.Join(flatten(p), "\n")
	for _, want := range []string{
		"new-window -d -t default:1 -n rcfiles -c /home/tim/rcfiles",
		"new-session -d -s net -n swcfg -c /home/tim/net",
		"split-window -d -t net:0 -c /home/tim",            // missing cwd → $HOME fallback (HOME=/home/tim in test via t.Setenv)
		`select-layout -t net:0 "L2"`,
		`send-keys -t net:0.0 'ssh' 'sw it'\''s' Enter`,
		`send-keys -t default:1.0 '/home/tim/bin/claude-resume' 'abc' Enter`,
		"select-window -t net:0",
		"select-window -t default:1",
	} {
		if !strings.Contains(cmds, want) {
			t.Errorf("plan missing %q\n%s", want, cmds)
		}
	}
	if strings.Contains(cmds, "rename-window -t default:0") || strings.Contains(cmds, "new-window -d -t default:0") {
		t.Error("seed window must not be touched")
	}
	if p.Created != 2 || p.Skipped != 1 || p.Relocated != 0 { // created rcfiles + net:0; default:0 h skipped (same name)
		t.Errorf("counts %+v", p)
	}
}

func TestPlanRelocatesOnConflict(t *testing.T) {
	live := LiveState{Sessions: map[string][]LiveWindow{"default": {{0, "h"}, {1, "tmux-restore"}}}}
	p := BuildPlan(live, snapNet(), Options{SeedSession: "default", SeedWindow: "h"})
	cmds := strings.Join(flatten(p), "\n")
	if strings.Contains(cmds, "rename-window -t default:1") || strings.Contains(cmds, "new-window -d -t default:1 ") {
		t.Fatalf("must never touch occupied window default:1\n%s", cmds)
	}
	if !strings.Contains(cmds, "new-window -d -t default: -n rcfiles") || p.Relocated != 1 {
		t.Fatalf("expected relocation\n%s %+v", cmds, p)
	}
}

func flatten(p Plan) []string {
	var out []string
	for _, a := range p.Actions {
		if a.Kind == "tmux" {
			out = append(out, a.Args[0])
		}
	}
	return out
}
```

(Add `t.Setenv("HOME", "/home/tim")` at the top of `TestPlanOnSeedServer`.)

- [ ] **Step 2: Run** `go test ./internal/restore/ -v` → FAIL undefined.

- [ ] **Step 3: Implement** `live.go` (`QueryLive`, `IsSeedOnly`) and `plan.go` per the rules above. Key helpers:

```go
func shellQuote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}

func cwdOrHome(cwd string) string {
	if fi, err := os.Stat(cwd); err == nil && fi.IsDir() {
		return cwd
	}
	h, _ := os.UserHomeDir()
	return h
}
```

`BuildPlan` emits, for a window it creates at target `T` (`sess:idx`): the `new-window`/`new-session` line (first pane's cwd), then `split-window -d -t T -c <cwdOrHome(pane_k)>` for k ≥ 1, then `select-layout -t T "<layout>"`, then for every pane k: if `o.Contents` and pane.ContentFile != "" an Action `{Kind:"contents", Args:[T+"."+k, paneKey]}`, then the `send-keys` line for `argv`/`claude`, then `select-pane -t T.<active>`; `set-window-option -t T automatic-rename off` when `!AutomaticRename`. Relocated windows: target is `sess:` (tmux picks the next index) — the applier must resolve the real index: emit `new-window -d -P -F "#{window_index}" -t sess: -n name -c cwd` and have the plan reference the window via a placeholder `{{WIN}}` that `Apply` substitutes from that command's reply. Define `const WinPlaceholder = "{{WIN}}"` in plan.go and document it in the Action.Note (`"relocated"`).

- [ ] **Step 4: Run** → PASS.

- [ ] **Step 5: Commit** `git add internal/restore && git commit -m "feat(restore): live-state query, seed-only detection, pure additive merge planner"`.

---

### Task 14: Restore applier + `restore` command + integration test

**Files:**
- Create: `internal/restore/apply.go`, `internal/restore/apply_test.go`, `internal/cli/restore.go`, `internal/cli/restore_integration_test.go`

**Interfaces:**
- Produces: `Report{Created, Relocated, Skipped int; Notes []string}`; `Apply(ctx, t tmuxctl.Transport, p Plan, contents func(paneKey string) ([]byte, bool)) (Report, error)` — runs each `tmux` action via `t.Run`; substitutes `WinPlaceholder` in subsequent actions of a relocated window with the index returned by the `-P -F` new-window; for `contents` actions writes the bytes to a temp file (0600) and runs `send-keys -t <target> "clear; cat <tmpfile>; rm <tmpfile>" Enter`… **no** — safer: `load-buffer -b gts <tmpfile>` then `paste-buffer -d -b gts -t <target>` (no shell involvement; pastes as if typed, which for a bare shell just scrolls the text). Use the load-buffer/paste-buffer path and delete the temp file after. Errors from individual actions are collected into `Notes` and do not abort (idempotent re-run completes the rest) except for the session/window-creating commands, which abort that session's remaining actions.
- CLI: `restore [--on-start] [--merge] [--snapshot DIR] [--no-contents] [--config P]`: `--on-start` → `QueryLive`, if not `IsSeedOnly` → print `skipped: server not seed-only`, exit 0; else plan+apply from `Store.Last()`; `--merge` (also the default when neither flag given) → plan+apply against the live state; prints `restored N sessions, M windows (R relocated, S skipped)`; events line `restore` with detail.

- [ ] **Step 1: Write the failing tests**

`apply_test.go` — with a `tmuxctl.Fake{Default: []string{}}` plus reply `{"new-window -d -P -F \"#{window_index}\" -t default: -n rcfiles -c /home/tim/rcfiles": {"7"}}`: build the relocation plan from Task 13's `snapNet()` and assert that `Apply` issued `send-keys -t default:7.0 …` (placeholder substituted) and `select-layout -t default:7 "L1"`, and that `contents` produced `load-buffer`+`paste-buffer -t net:0.0` calls with the temp file removed afterwards.

`restore_integration_test.go` (package `cli`, uses `tmuxctl.StartTestServer`):
```go
func TestSaveRestoreRoundTrip(t *testing.T) {
	sock := tmuxctl.StartTestServer(t)
	run := func(args ...string) { exec.Command("tmux", append([]string{"-L", sock}, args...)...).Run() }
	run("new-window", "-d", "-t", "default:1", "-n", "editor", "-c", "/tmp")
	run("split-window", "-d", "-t", "default:1", "-c", "/")
	run("new-session", "-d", "-s", "net", "-n", "swcfg", "-c", "/tmp")
	run("send-keys", "-t", "net:0", "tail -f /dev/null", "Enter")
	time.Sleep(300 * time.Millisecond)

	dataDir := t.TempDir()
	cfgFile := writeTestConfig(t, sock) // socket=sock, seed_session=default, seed_window=h, allowlist incl. tail
	t.Setenv("XDG_DATA_HOME", dataDir)
	if code := Run([]string{"save", "--config", cfgFile, "--no-display"}, io.Discard, os.Stderr); code != 0 {
		t.Fatal("save failed")
	}
	run("kill-session", "-t", "net")
	run("kill-window", "-t", "default:1")
	if code := Run([]string{"restore", "--on-start", "--config", cfgFile}, io.Discard, os.Stderr); code != 0 {
		t.Fatal("restore failed")
	}
	out, _ := exec.Command("tmux", "-L", sock, "list-windows", "-a", "-F", "#{session_name}:#{window_index} #{window_name} #{window_panes}").Output()
	got := string(out)
	for _, want := range []string{"default:0 h 1", "default:1 editor 2", "net:0 swcfg 1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("after restore missing %q:\n%s", want, got)
		}
	}
	time.Sleep(500 * time.Millisecond)
	cmd, _ := exec.Command("tmux", "-L", sock, "display-message", "-p", "-t", "net:0.0", "#{pane_current_command}").Output()
	if strings.TrimSpace(string(cmd)) != "tail" {
		t.Fatalf("tail should have been relaunched in net:0.0, got %q", cmd)
	}
}
```

- [ ] **Step 2: Run** both → FAIL.

- [ ] **Step 3: Implement** `apply.go` and `cli/restore.go` as specified. In `restore.go`, `--on-start` uses `restore.QueryLive` + `restore.IsSeedOnly(live, cfg.SeedSession, cfg.SeedWindow)`; options `Contents: cfg.Contents.Enabled && !noContents`, `ClaudeResumePath` expanded (`~` → home). Contents reader: `store.ReadContent(lastDir, pane)` looked up by pane key → build a `map[paneKey]snapshot.Pane` from the snapshot first.

- [ ] **Step 4: Run** `go test ./internal/restore/ ./internal/cli/ -v` → PASS (the integration test is skipped where tmux is absent).

- [ ] **Step 5: Commit** `git commit -m "feat(restore): applier with placeholder resolution and buffer-based contents replay; restore command; save/restore round-trip integration test"`.

---

### Task 15: `status` (+ `--check-fresh`, `--json`) and `prune` commands

**Files:**
- Create: `internal/cli/status.go`, `internal/cli/status_test.go`, `internal/cli/prune.go`

**Interfaces:**
- Produces: `status [--json] [--check-fresh] [-n N]` prints: last good save time + age, the last N events (default 10), timer state (`systemctl --user is-active go-tmux-saver.timer` via the injectable `Systemctl` func from Task 16 — until Task 16 lands, status prints `timer: unknown`), data dir + snapshot count. `--check-fresh` exits 1 when `LastGood` is absent or older than `interval × watch_stale_factor`, printing `STALE: last good save <age> ago (limit <limit>)`. `--json` emits `{"last_good":"…","age_seconds":N,"stale":bool,"events":[…],"snapshots":N}`. `prune [--dry-run]` runs `snapshot.Prune` with config retention and prints removed names.

- [ ] **Step 1: Test** — create a temp data dir, `AppendEvent` two events, `TouchFresh` then backdate the marker with `os.Chtimes` to 2 hours ago; assert `Run([]string{"status","--check-fresh","--config",cfg})` returns 1 and stdout contains `STALE`; with a fresh marker returns 0; `--json` parses and `stale` matches.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement** (`RunStatus(w io.Writer, dataDir string, cfg config.Config, asJSON, checkFresh bool, n int, now time.Time) int`). **Step 4: Run** → PASS. **Step 5: Commit** `feat(cli): status with freshness check and JSON output; prune command`.

---

### Task 16: `setup` — templates, render, install, validate, update

**Files:**
- Create: `internal/setup/templates/go-tmux-saver.service.tmpl`, `go-tmux-saver.timer.tmpl`, `go-tmux-saver-watch.service.tmpl`, `go-tmux-saver-watch.timer.tmpl`, `go-tmux-saver-alert@.service.tmpl`, `tmux-server-dropin.conf.tmpl`, `tmux.conf.tmpl`; `internal/setup/render.go`, `env.go`, `install.go`, `validate.go`, `setup_test.go`; `internal/cli/setup.go`

**Interfaces:**
- Produces:
```go
type Params struct { Version, Binary, Socket, SeedSession, SeedWindow string; IntervalMinutes int; MailTo string }
type Managed struct { Rel string /* path relative to ConfigHome */; Content []byte; Mode os.FileMode }
func Render(p Params) ([]Managed, error)   // deterministic order; every file starts with the managed header
type Env struct { ConfigHome string; Systemctl func(args ...string) (string, error); TmuxBindings func() (string, error); Stdout io.Writer }
func Install(env Env, files []Managed) error            // atomic write each, daemon-reload, enable --now timers
func Validate(env Env, files []Managed) []Drift          // Drift{Path, Kind string; Diff string}; Kind ∈ missing|differs|mode|unit-inactive|unit-invalid|dropin-missing|keybinding-missing
func Update(env Env, files []Managed, dryRun bool) (changed []string, err error)
const Header = "# managed by go-tmux-saver %s — edit config.json, not this file; run 'go-tmux-saver setup update'\n"
```
- Template content (exact):

`go-tmux-saver.service.tmpl`:
```
{{.Header}}[Unit]
Description=go-tmux-saver periodic snapshot
After=tmux-server.service
OnFailure=go-tmux-saver-alert@%n.service

[Service]
Type=oneshot
ExecStart={{.Binary}} save --auto --no-display
```
`go-tmux-saver.timer.tmpl`:
```
{{.Header}}[Unit]
Description=go-tmux-saver periodic snapshot timer

[Timer]
OnBootSec=2min
OnUnitActiveSec={{.IntervalMinutes}}min
AccuracySec=1min
Persistent=true

[Install]
WantedBy=timers.target
```
`go-tmux-saver-watch.service.tmpl`: `ExecStart={{.Binary}} status --check-fresh`, `OnFailure=go-tmux-saver-alert@%n.service`, Type=oneshot. `go-tmux-saver-watch.timer.tmpl`: `OnBootSec=15min`, `OnUnitActiveSec=1h`, `Persistent=true`, WantedBy timers.target. `go-tmux-saver-alert@.service.tmpl`: `Type=oneshot`, `ExecStart={{.Binary}} alert --unit %i`. `tmux-server-dropin.conf.tmpl` (Rel `systemd/user/tmux-server.service.d/50-go-tmux-saver.conf`):
```
{{.Header}}[Service]
ExecStartPost={{.Binary}} restore --on-start
```
`tmux.conf.tmpl` (Rel `go-tmux-saver/tmux.conf`):
```
{{.Header}}bind-key M-s run-shell "{{.Binary}} save"
bind-key M-r run-shell "{{.Binary}} restore --merge"
```
- `Validate` checks, per file: exists / bytes equal / mode 0644 (units) or 0600 (config); `env.Systemctl("--user","is-enabled","go-tmux-saver.timer")` and `is-active` for both timers; `systemd-analyze --user verify <unit paths>` via `env.Systemctl` wrapper (same injectable); `env.Systemctl("--user","show","tmux-server.service","-p","ExecStartPost")` must contain `restore --on-start`; `env.TmuxBindings()` output (from `tmux list-keys`) must contain `M-s` → `go-tmux-saver save` and `M-r` → `go-tmux-saver restore --merge`.
- `Install`: `MkdirAll`, atomic write, `Systemctl("--user","daemon-reload")`, `Systemctl("--user","enable","--now","go-tmux-saver.timer","go-tmux-saver-watch.timer")`. `Update`: compute drift, if dry-run print diff; else `Install` + `Systemctl("--user","restart", both timers)`.

- [ ] **Step 1: Tests** (`setup_test.go`): (a) `Render` golden — each file begins with the header and the service file contains `ExecStart=/usr/bin/go-tmux-saver save --auto --no-display`; (b) `Install` into a temp ConfigHome with a recording fake `Systemctl` — files land at the right relative paths with right modes and the fake saw `daemon-reload` then `enable --now …`; (c) `Validate` returns no drift right after Install (fake `Systemctl` returns `enabled`/`active`/`ExecStartPost={ path=… ; argv[]=… restore --on-start …}`; fake `TmuxBindings` returns two matching lines), then after corrupting one file reports exactly one `differs` drift with a non-empty diff, and after deleting the drop-in reports `missing`; (d) `Update --dry-run` changes nothing and lists the differing file; `Update` rewrites it and restarts timers.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement** per above (embed with `//go:embed templates/*.tmpl`; diff via a simple line-based unified-ish diff helper `diffLines(a, b string) string`). `cli/setup.go` registers `setup generate|install|validate|update` with `--dir` for generate and real `Systemctl = exec("systemctl", …)`, `TmuxBindings = tmux -L <socket> list-keys`. Binary path = `os.Executable()` resolved via `filepath.EvalSymlinks`. **Step 4: Run** → PASS. **Step 5: Commit** `feat(setup): embedded templates, render/install/validate/update with injectable systemctl`.

---

### Task 17: `alert` (sendmail, rate limit) and the `status` timer hook-up

**Files:**
- Create: `internal/mail/mail.go`, `internal/mail/mail_test.go`, `internal/cli/alert.go`

**Interfaces:**
- Produces: `mail.Send(sendmail func(body []byte) error, to, subject, body string) error` (builds RFC-822 headers `To:`, `Subject:`, `Content-Type: text/plain; charset=utf-8`); `mail.RateLimiter{Dir string}` with `ShouldSend(key string, now time.Time) bool` — true on the first failure of a streak (creates `<Dir>/alert-<key>`) and false until `Clear(key)`; `Clear(key)` removes the marker and returns whether it existed (→ send one recovery mail). CLI `alert --unit NAME [--recovered]`: body = `status` text output + last 20 event lines; subject `[go-tmux-saver] <host>: <unit> failed` / `recovered`; uses `exec.Command("sendmail","-t")` with body on stdin. `save --auto` success path calls `RateLimiter.Clear("go-tmux-saver.service")` and, if it was set, sends the recovery mail.
- [ ] **Step 1: Tests** — `Send` builds the message correctly (captured by a fake sendmail func); `RateLimiter` returns true, false, false; `Clear` → true then `ShouldSend` true again.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS. **Step 5: Commit** `feat(alert): sendmail alerts with per-unit rate limiting and recovery mail`.

---

### Task 18: `import-resurrect`

**Files:**
- Create: `internal/importer/resurrect.go`, `internal/importer/resurrect_test.go`, `internal/importer/testdata/tmux_resurrect_sample.txt`, `internal/importer/testdata/pane_contents.tar.gz`, `internal/cli/importer.go`

**Interfaces:**
- Produces: `importer.FromResurrect(savePath string, contentsTar string /* "" = none */, claudeResumePath string) (*snapshot.Snapshot, map[string][]byte, error)`. Parses resurrect's tab-separated lines: `pane\t<session>\t<win>\t<win_active>\t<flags>\t<pane_idx>\t<title>\t<dir>\t<pane_active>\t<pane_cmd>\t<full_cmd>` (dir and full_cmd prefixed with `:`), `window\t<session>\t<win>\t<win_active>\t<flags>\t<layout>\t<automatic-rename>`, `state\t<client_session>\t<client_last_session>`, `grouped_session` lines skipped. Restore mapping: full command empty → `shell`; matches `claude-resume <uuid>`/`--resume <uuid>` → `claude`; otherwise → `argv` split on whitespace (resurrect stored it as a shell string; best effort). Pane contents: resurrect's tar entries are named `tmux_resurrect_<sess>:<win>.<pane>.txt` (base64/escaped session names per resurrect's `_get_pane_contents` naming — the sample fixture must be made from a real save on ten64 and the test assert the mapping for it) → keyed by `PaneKey`. CLI `import-resurrect <savefile> [--contents TAR] [--promote]` stages the converted snapshot and promotes it (default) so it becomes `last`.
- [ ] **Step 1: Test** on a fixture copied from ten64's current `last` (trim to 2 sessions / 4 panes, scrub nothing sensitive) asserting session/window/pane counts, a `claude` restore with its uuid, an `argv` ssh restore, layout strings and active markers, and that contents from the tarball land under the right keys.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS. **Step 5: Commit** `feat(import): one-time tmux-resurrect save + pane_contents converter`.

---

### Task 19: End-to-end CI run, README usage, tag v0.1

**Files:**
- Modify: `README.md`, `.github/workflows/test.yml` (already runs `go test ./...` incl. integration tests with tmux installed)

- [ ] **Step 1:** Run the full suite locally: `go vet ./... && go test -race ./...` → all PASS. Build: `CGO_ENABLED=0 go build -ldflags "-s -w -X github.com/mithro/go-tmux-saver/internal/cli.Version=$(git describe --tags)" ./cmd/go-tmux-saver` and smoke it against the real server **read-only**: `./go-tmux-saver save --config /dev/null --no-display` with `XDG_DATA_HOME=$(mktemp -d)` pointing at a scratch dir (so the live resurrect data is untouched) — verify it reports `kept panes=<current count>` in well under a second, and `./go-tmux-saver restore --on-start` against the live server prints `skipped: server not seed-only`.
- [ ] **Step 2:** README: install/build, the five subcommands with one-line examples, pointer to spec/plan, and "rollout is Plan 2".
- [ ] **Step 3:** Commit `docs: README usage` ; push `main`; confirm the GitHub Actions `test` workflow is green; tag `v0.1` (`git tag v0.1 && git push origin v0.1` — allowed by the `vXX.ZZZ` ruleset).

---

## Self-review against the spec

- **§3 architecture** → Tasks 1–3 (binary, control mode), 4/8 (/proc), 12/14 (save/restore), 16 (units via setup). ✔
- **§4 save engine**: collection commands (T9), grouped-clone skip (T9), process resolution incl. registry/procStart/argv fallback/allowlist (T4, T8), schema (T5), per-pane files + hardlinks + structural naming (T6), codec registry (T5), atomic tmp→rename + last swap (T6), guard + rejected/ + contents-together promotion (T6, T12), unchanged rule incl. ✳ titles (T10), events log format (T7), freshness marker (T7), retention (T7), performance budget verified in T19 smoke. ✔ — *Gap found & fixed*: registry `"tmux"` pane-field cross-check from §4 is not implemented; it is a belt-and-braces check only, and `procStart` validation covers the stale-pid risk. Documented as a deliberate omission here; add later if a mismatch is ever observed.
- **§5 trigger/restore**: timer/service/OnFailure units (T16 templates), `--auto` exit semantics incl. `skipped` (T12), watchdog `status --check-fresh` + watch timer (T15, T16), drop-in `ExecStartPost` (T16), seed-only gate (T13), merge rules incl. relocate/skip/never-rename (T13), panes/layout/cwd fallback/send-keys/claude placeholder (T13/T14), contents replay (T14 — via load-buffer/paste-buffer rather than `cat`, noted), client session select (T13), idempotency (T14 notes/continue), manual keys + display-message (T12, T16). ✔
- **§6 setup**: generate/install/validate/update, managed header, drift diff, systemd-analyze verify, drop-in visible, keybindings check, config.json keys (T11, T16). ✔ — config.json is *generated* by `setup generate` as `go-tmux-saver/config.json` from `config.Default()` (add to T16's `Render` output list: `Rel: "go-tmux-saver/config.json"`, mode 0600, JSON of defaults — **not** overwritten by `update` if it exists; `validate` only checks it parses and validates).
- **§7 repo/distribution/rollout** → Plan 2 (CI release, nfpm, apt repo, ansible role, rcfiles change); the test workflow (T1) and `v0.1` tag (T19) are in this plan. ✔
- **§8 testing**: recorded transcript (T2), fake transport (T3), table tests (T4–T13), integration round-trip on a throwaway socket (T14), setup with injectable systemctl (T16). ✔
- **§9 failure handling**: temp+fsync+rename (T6), outcomes/exit codes (T12), sendmail + rate limit + recovery (T17), status --json (T15), 0700/0600 (T6), idempotent restore (T14). ✔

**Placeholder scan:** none of "TBD/TODO/handle edge cases" remain; Tasks 11, 15–18 give exact behaviours, signatures and test assertions in prose where full code would be repetitive — implementers must still write the failing tests first exactly as described.

**Type consistency:** `snapshot.Restore{Kind, Argv, ClaudeSession}` used identically in T5/T8/T9/T13/T18; `PaneKey` everywhere for contents maps; `collect.SessCmd/WinCmd/PaneCmd/ServerCmd` reused by T12's fake; `tmuxctl.Fake{Replies, Default, Calls}` as defined in T3; `config.Config` field names match T16 `Params` sources.
