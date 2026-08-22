package collect_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mithro/go-tmux-saver/internal/collect"
	"github.com/mithro/go-tmux-saver/internal/procs"
	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

// envInt reads a positive integer from the environment, or returns def.
func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// startPanes brings up a throwaway tmux server with n windows (one pane
// each), every pane holding at least lines of scrollback, and returns its
// socket name. The server is killed via t.Cleanup. It skips when tmux is
// unavailable so `go test ./...` stays green on machines without it.
//
// Panes are filled by a single printf per pane (one fork, not one per line)
// and the helper then polls #{history_size} until every pane has filled,
// rather than sleeping a guessed interval.
func startPanes(t testing.TB, n, lines int) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sock := fmt.Sprintf("gts-bench-%d-%s", os.Getpid(), strings.ReplaceAll(t.Name(), "/", "_"))
	tmux := func(args ...string) ([]byte, error) {
		return exec.Command("tmux", append([]string{"-L", sock}, args...)...).CombinedOutput()
	}
	// -f /dev/null: never load the invoking user's ~/.tmux.conf. On a real
	// workstation that config can pull in plugin managers, hooks and
	// tmux-continuum (which would then autosave/destroy this throwaway
	// server out from under the benchmark, producing "server exited
	// unexpectedly" flakes).
	if out, err := tmux("-f", "/dev/null", "new-session", "-d", "-s", "default", "-n", "w0", "-x", "200", "-y", "50", "bash --noprofile --norc"); err != nil {
		t.Fatalf("new-session on %s: %v: %s", sock, err, out)
	}
	t.Cleanup(func() { exec.Command("tmux", "-L", sock, "kill-server").Run() })
	if out, err := tmux("set-option", "-g", "history-limit", strconv.Itoa(lines*2+1000)); err != nil {
		t.Fatalf("history-limit: %v: %s", err, out)
	}
	for i := 1; i < n; i++ {
		if out, err := tmux("new-window", "-d", "-n", fmt.Sprintf("w%d", i), "bash --noprofile --norc"); err != nil {
			t.Fatalf("new-window %d: %v: %s", i, err, out)
		}
	}
	// One printf per pane emits `lines` lines of ~120 columns. Only lines
	// that scroll off the 50-row screen land in the history, so emit a
	// screenful extra to guarantee `lines` of scrollback.
	fill := fmt.Sprintf("printf 'line %%d %s\\n' $(seq 1 %d)", strings.Repeat("x", 100), lines+60)
	out, err := tmux("list-panes", "-a", "-F", "#{pane_id}")
	if err != nil {
		t.Fatalf("list-panes: %v: %s", err, out)
	}
	ids := strings.Fields(string(out))
	for _, id := range ids {
		if out, err := tmux("send-keys", "-t", id, fill, "Enter"); err != nil {
			t.Fatalf("send-keys %s: %v: %s", id, err, out)
		}
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, err := tmux("list-panes", "-a", "-F", "#{history_size}")
		if err != nil {
			t.Fatalf("list-panes: %v: %s", err, out)
		}
		done := 0
		for _, f := range strings.Fields(string(out)) {
			if h, _ := strconv.Atoi(f); h >= lines {
				done++
			}
		}
		if done == len(ids) {
			return sock
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d panes reached %d scrollback lines", done, len(ids), lines)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func benchCollector(t testing.TB, sock string) (*collect.Collector, func()) {
	t.Helper()
	c, err := tmuxctl.Dial(context.Background(), sock, "default")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	tb, err := procs.Scan("/proc")
	if err != nil {
		t.Fatalf("procs.Scan: %v", err)
	}
	return &collect.Collector{T: c, Procs: tb, Allowlist: procs.DefaultAllowlist, Host: "bench"}, func() { c.Close() }
}

// BenchmarkCollect measures a full collection (list-* plus one capture-pane
// per pane) against a throwaway server. Size is tunable:
//
//	GTS_BENCH_PANES=50 GTS_BENCH_LINES=1000 go test ./internal/collect -run x -bench Collect -benchtime 1x
//
// The reported bytes/op and lines/op metrics are the scrollback tmux handed
// back, which is what the wall time actually scales with.
func BenchmarkCollect(b *testing.B) {
	panes := envInt("GTS_BENCH_PANES", 50)
	lines := envInt("GTS_BENCH_LINES", 200)
	sock := startPanes(b, panes, lines)
	c, closeFn := benchCollector(b, sock)
	defer closeFn()

	var bytes, outLines float64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, contents, err := c.Collect(context.Background())
		if err != nil {
			b.Fatalf("collect: %v", err)
		}
		bytes, outLines = 0, 0
		for _, v := range contents {
			bytes += float64(len(v))
			outLines += float64(strings.Count(string(v), "\n"))
		}
	}
	b.ReportMetric(bytes, "capture-bytes/op")
	b.ReportMetric(outLines, "capture-lines/op")
	b.ReportMetric(float64(panes), "panes")
}

// BenchmarkRunLatency measures the per-command round trip on one control-mode
// connection with a trivial command, isolating transport cost from the cost
// of whatever tmux has to compute.
func BenchmarkRunLatency(b *testing.B) {
	sock := startPanes(b, 1, 1)
	c, err := tmuxctl.Dial(context.Background(), sock, "default")
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	defer c.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Run(context.Background(), "display-message -p x"); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// TestCollectManyPanes is the always-on integration guard for the benchmark
// path: it checks a multi-pane collection returns every pane's scrollback,
// so the benchmark helper above cannot silently rot.
func TestCollectManyPanes(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	sock := startPanes(t, 4, 50)
	c, closeFn := benchCollector(t, sock)
	defer closeFn()
	snap, contents, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	panes, _ := snap.CountPanes()
	if panes != 4 {
		t.Fatalf("panes = %d, want 4", panes)
	}
	if len(contents) != 4 {
		t.Fatalf("contents = %d, want 4", len(contents))
	}
	for k, v := range contents {
		if n := strings.Count(string(v), "\n"); n < 50 {
			t.Errorf("pane %s: %d captured lines, want >= 50", k, n)
		}
	}
}
