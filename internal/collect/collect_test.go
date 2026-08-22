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
		SessCmd: {"default\t0\t1", "default-1\t1\t1", "net\t0\t0"},
		WinCmd:  {"default\t0\th\t1\t*\tbfbf,80x24,0,0,0\ton", "default-1\t0\th\t1\t*\tx\ton", "net\t2\tswcfg\t1\t*\tdead,80x24,0,0{40x24,0,0,1,39x24,41,0,2}\toff"},
		PaneCmd: {"default\t0\t0\t%0\t1\t100\t/home/tim\ttim@ten64: ~\t3", "default-1\t0\t0\t%0\t1\t100\t/home/tim\tx\t3",
			"net\t2\t0\t%1\t1\t300\t/home/tim/net\t✳ switch config\t2", "net\t2\t1\t%2\t0\t200\t/home/tim\tten64\t0"},
		ServerCmd:                       {"1787201600\tnext-3.8\tdefault"},
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
