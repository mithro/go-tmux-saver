package collect

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mithro/go-tmux-saver/internal/procs"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

func fakeServer() *tmuxctl.Fake {
	return &tmuxctl.Fake{Replies: map[string][]string{
		SessCmd: {"default\tdefault\t1\t1", "default-1\tdefault\t1\t1", "net\t\t0\t0"},
		WinCmd:  {"default\t0\th\t1\t*\tbfbf,80x24,0,0,0\ton", "default-1\t0\th\t1\t*\tx\ton", "net\t2\tswcfg\t1\t*\tdead,80x24,0,0{40x24,0,0,1,39x24,41,0,2}\toff"},
		PaneCmd: {"default\t0\t0\t%0\t1\t100\t/home/tim\ttim@ten64: ~\t3", "default-1\t0\t0\t%0\t1\t100\t/home/tim\tx\t3",
			"net\t2\t0\t%1\t1\t300\t/home/tim/net\t✳ switch config\t2", "net\t2\t1\t%2\t0\t200\t/home/tim\tten64\t0"},
		ServerCmd: {"1787201600\tnext-3.8\t/dev/pts/99"},
		ClientsCmd: {
			"1787201000\t/dev/pts/1\tdefault",
			"1787201500\t/dev/pts/2\tnet",      // most recently active real client
			"1787209999\t/dev/pts/99\tdefault", // us: the control client, newest but excluded
		},
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
	snap, contents, _, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 2 { // default-1 is a grouped clone → skipped
		t.Fatalf("sessions = %+v", snap.Sessions)
	}
	if snap.TmuxVersion != "next-3.8" || snap.ServerStart != 1787201600 || snap.Client.Session != "net" || snap.Host != "ten64" {
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

func newCollector(f *tmuxctl.Fake) *Collector {
	tb, _ := procs.Scan("../procs/testdata/proc")
	return &Collector{T: f, Procs: tb, Reg: procs.ClaudeRegistry{Dir: "../procs/testdata/sessions"},
		Allowlist: procs.DefaultAllowlist, Host: "ten64", Now: func() time.Time { return time.Unix(100, 0).UTC() }}
}

// (i) a pane_title containing an embedded tab must still parse as one
// field, and the tab must not shift history_size (which drives capture
// depth) into a title fragment.
func TestCollectPaneTitleWithEmbeddedTab(t *testing.T) {
	f := &tmuxctl.Fake{Replies: map[string][]string{
		ServerCmd:                       {"100\tnext-3.8\tour-control-client"},
		ClientsCmd:                      {},
		SessCmd:                         {"default\t\t0\t1"},
		WinCmd:                          {"default\t0\th\t1\t*\tlayout\ton"},
		PaneCmd:                         {"default\t0\t0\t%0\t1\t100\t/home/tim\thas\ttab in it\t5"},
		"capture-pane -epJ -S -5 -t %0": {"z"},
	}}
	c := newCollector(f)
	snap, contents, _, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pane := snap.Sessions[0].Windows[0].Panes[0]
	if pane.Title != "has\ttab in it" {
		t.Fatalf("title = %q, want embedded tab preserved", pane.Title)
	}
	if pane.HistoryLines != 5 {
		t.Fatalf("history lines = %d, want 5", pane.HistoryLines)
	}
	if string(contents["default_0_0"]) != "z\n" {
		t.Fatalf("contents = %q", contents["default_0_0"])
	}
}

// (ii) a pane line with too few fields must fail loudly, not silently.
func TestCollectMalformedPaneLineErrors(t *testing.T) {
	f := &tmuxctl.Fake{Replies: map[string][]string{
		ServerCmd:  {"100\tnext-3.8\tour-control-client"},
		ClientsCmd: {},
		SessCmd:    {"default\t\t0\t1"},
		WinCmd:     {"default\t0\th\t1\t*\tlayout\ton"},
		PaneCmd:    {"default\t0\t0\t%0\t1\t100\t/home/tim"}, // only 7 of 9 fields
	}}
	c := newCollector(f)
	if _, _, _, err := c.Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("err = %v, want a malformed-line error", err)
	}
}

// (iii) a transport error on the server-info command must propagate.
func TestCollectServerInfoErrorPropagates(t *testing.T) {
	f := &tmuxctl.Fake{Replies: map[string][]string{}} // ServerCmd absent, Default nil → error
	c := newCollector(f)
	if _, _, _, err := c.Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "server info") {
		t.Fatalf("err = %v, want wrapped server info error", err)
	}
}

// (iv) a TRANSPORT-level error on capture-pane (connection closed/desynced/
// context — anything that is not tmux answering with a %error block) must
// still propagate: the connection is unusable, so the whole save aborts.
// RULING R48 only downgrades tmux's own per-command %error.
func TestCollectCapturePaneTransportErrorPropagates(t *testing.T) {
	f := &captureFailer{
		Fake: &tmuxctl.Fake{Replies: map[string][]string{
			ServerCmd:  {"100\tnext-3.8\tour-control-client"},
			ClientsCmd: {},
			SessCmd:    {"default\t\t0\t1"},
			WinCmd:     {"default\t0\th\t1\t*\tlayout\ton"},
			PaneCmd:    {"default\t0\t0\t%0\t1\t100\t/home/tim\ttitle\t3"},
		}},
		failCmd: "capture-pane -epJ -S -3 -t %0",
		err:     errors.New("control connection closed"),
	}
	c := newCollector(nil)
	c.T = f
	if _, _, _, err := c.Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "capture") {
		t.Fatalf("err = %v, want wrapped capture error", err)
	}
}

// captureFailer serves everything from the embedded Fake except failCmd,
// which fails with err — for driving one pane's capture-pane into an error
// while the rest of the collection succeeds.
type captureFailer struct {
	*tmuxctl.Fake
	failCmd string
	err     error
}

func (f *captureFailer) Run(ctx context.Context, cmd string) ([]string, error) {
	if cmd == f.failCmd {
		f.Fake.Calls = append(f.Fake.Calls, cmd)
		return nil, f.err
	}
	return f.Fake.Run(ctx, cmd)
}

// TestCollectCapturePaneCmdErrorWarnsAndContinues covers I7/RULING R48: a
// pane that vanishes mid-save makes capture-pane answer with a tmux %error.
// That must NOT abort the save (the other 40 panes are fine) — the pane is
// recorded with no contents and a warning is returned to the caller, which
// puts it in the save event's detail.
func TestCollectCapturePaneCmdErrorWarnsAndContinues(t *testing.T) {
	gone := "capture-pane -epJ -S -3 -t %57"
	f := &captureFailer{
		Fake: &tmuxctl.Fake{Replies: map[string][]string{
			ServerCmd:                       {"100\tnext-3.8\tour-control-client"},
			ClientsCmd:                      {},
			SessCmd:                         {"default\t\t0\t1"},
			WinCmd:                          {"default\t0\th\t1\t*\tlayout\ton"},
			PaneCmd:                         {"default\t0\t0\t%0\t1\t100\t/home/tim\tok\t3", "default\t0\t1\t%57\t0\t100\t/home/tim\tgone\t3"},
			"capture-pane -epJ -S -3 -t %0": {"still", "here"},
		}},
		failCmd: gone,
		err:     &tmuxctl.CmdError{Cmd: gone, Lines: []string{"can't find pane: %57"}},
	}
	c := newCollector(nil)
	c.T = f

	snap, contents, warnings, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect must not abort on a per-pane %%error: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "%57") || !strings.Contains(warnings[0], "capture failed") {
		t.Fatalf("warnings = %q, want one mentioning pane %%57's failed capture", warnings)
	}
	if string(contents["default_0_0"]) != "still\nhere\n" {
		t.Errorf("healthy pane's contents = %q, want them captured normally", contents["default_0_0"])
	}
	if _, ok := contents["default_0_1"]; ok {
		t.Errorf("the vanished pane must have no contents entry, got %q", contents["default_0_1"])
	}
	panes, _ := snap.CountPanes()
	if panes != 2 {
		t.Fatalf("panes = %d, want 2 (the vanished pane is still recorded, just without contents)", panes)
	}
}

// (v) no OTHER client attached (only our own control connection) must not
// be treated as an error — Client.Session is simply empty.
func TestCollectNoOtherClientNoError(t *testing.T) {
	f := &tmuxctl.Fake{Replies: map[string][]string{
		ServerCmd:  {"1\tnext-3.8\t/dev/pts/99"},
		ClientsCmd: {"1787200000\t/dev/pts/99\tdefault"}, // only us
		SessCmd:    {},
		WinCmd:     {},
		PaneCmd:    {},
	}}
	c := newCollector(f)
	snap, _, _, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Client.Session != "" {
		t.Fatalf("client session = %q, want empty (our own control client must be excluded)", snap.Client.Session)
	}
	if snap.ServerStart != 1 || snap.TmuxVersion != "next-3.8" {
		t.Fatalf("server fields %+v", snap)
	}
}

// TestCollectClientSessionIsMostRecentNonControlClient covers I5/RULING
// R44: Client.Session used to come from display-message's
// "#{client_session}", which — run over OUR OWN control connection — always
// reported the seed session, never a user's. It is now the session of the
// most-recently-active client that is NOT our control client, and stays
// purely informational (nothing in restore reads it).
func TestCollectClientSessionIsMostRecentNonControlClient(t *testing.T) {
	f := &tmuxctl.Fake{Replies: map[string][]string{
		ServerCmd: {"1\tnext-3.8\t/dev/pts/99"},
		ClientsCmd: {
			"1787200000\t/dev/pts/1\tolder",
			"1787200500\t/dev/pts/2\tnewer\twith\ttab", // session names may contain tabs
			"1787299999\t/dev/pts/99\tseed",            // us: newest, must be ignored
		},
		SessCmd: {},
		WinCmd:  {},
		PaneCmd: {},
	}}
	c := newCollector(f)
	snap, _, _, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Client.Session != "newer\twith\ttab" {
		t.Fatalf("client session = %q, want the most-recently-active non-control client's session", snap.Client.Session)
	}
}

// TestCollectKeepsOneGroupedSessionMember covers issue #12, with fixture
// lines shaped like the real ten64 incident: EVERY member of a tmux session
// group reports session_grouped=1 (there is no "original" flagged 0), so
// the old skip-if-grouped rule silently dropped the whole group — 6 windows
// including live Claude sessions. Exactly one canonical member must survive
// (name == group name when present, else lexically smallest), with its
// windows and panes; the clones' duplicate window/pane lines stay excluded.
func TestCollectKeepsOneGroupedSessionMember(t *testing.T) {
	f := &tmuxctl.Fake{Replies: map[string][]string{
		SessCmd: {
			"433mhz\t\t0\t0",
			"default\tdefault\t1\t1",
			"default-36\tdefault\t1\t1",
			"orphan-2\torphan\t1\t0", // group whose eponymous member is gone
			"orphan-9\torphan\t1\t0",
		},
		WinCmd: {
			"433mhz\t0\tesp32\t1\t*\tL1\toff",
			"default\t0\th\t1\t*\tL2\ton",
			"default\t1\ttmux-restore\t0\t\tL3\toff",
			"default-36\t0\th\t1\t*\tL2\ton",
			"default-36\t1\ttmux-restore\t0\t\tL3\toff",
			"orphan-2\t0\tw\t1\t*\tL4\toff",
			"orphan-9\t0\tw\t1\t*\tL4\toff",
		},
		PaneCmd: {
			"433mhz\t0\t0\t%1\t1\t100\t/home/tim\tt\t0",
			"default\t0\t0\t%2\t1\t100\t/home/tim\tt\t0",
			"default\t1\t0\t%3\t1\t100\t/home/tim\tt\t0",
			"default-36\t0\t0\t%2\t1\t100\t/home/tim\tt\t0",
			"default-36\t1\t0\t%3\t1\t100\t/home/tim\tt\t0",
			"orphan-2\t0\t0\t%4\t1\t100\t/home/tim\tt\t0",
			"orphan-9\t0\t0\t%4\t1\t100\t/home/tim\tt\t0",
		},
		ServerCmd:                       {"1787201600\tnext-3.8\t/dev/pts/99"},
		ClientsCmd:                      {},
		"capture-pane -epJ -S -0 -t %1": {""},
		"capture-pane -epJ -S -0 -t %2": {""},
		"capture-pane -epJ -S -0 -t %3": {""},
		"capture-pane -epJ -S -0 -t %4": {""},
	}}
	tb, _ := procs.Scan("../procs/testdata/proc")
	c := &Collector{T: f, Procs: tb, Reg: procs.ClaudeRegistry{Dir: "../procs/testdata/sessions"},
		Allowlist: procs.DefaultAllowlist, Host: "h", Now: func() time.Time { return time.Unix(100, 0).UTC() }}
	snap, _, _, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, se := range snap.Sessions {
		names = append(names, se.Name)
	}
	want := []string{"433mhz", "default", "orphan-2"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("sessions = %v, want %v (one canonical member per group)", names, want)
	}
	if len(snap.Sessions[1].Windows) != 2 || snap.Sessions[1].Windows[1].Name != "tmux-restore" {
		t.Fatalf("default windows = %+v, want both group windows kept once", snap.Sessions[1].Windows)
	}
	if n, _ := snap.CountPanes(); n != 4 {
		t.Fatalf("panes = %d, want 4 (no clone double-counting)", n)
	}
}
