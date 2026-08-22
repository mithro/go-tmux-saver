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
		collect.SessCmd:                 {"default\t0\t1"},
		collect.WinCmd:                  {"default\t0\tw\t1\t*\tL\ton"},
		collect.PaneCmd:                 {"default\t0\t0\t%0\t1\t100\t/home/tim\tt\t1", "default\t0\t1\t%1\t0\t300\t/home/tim\tt\t1"},
		collect.ServerCmd:               {"1\tnext-3.8\tdefault"},
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
