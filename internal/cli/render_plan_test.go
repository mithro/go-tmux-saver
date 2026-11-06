package cli

import (
	"strings"
	"testing"

	"github.com/mithro/go-tmux-saver/internal/restore"
)

// TestRenderPlanDeadPane: --dry-run must show what happens to a dead pane
// (issue #55) instead of silently omitting it, distinguishing a pane that
// will show saved output from one that comes back dead and empty.
func TestRenderPlanDeadPane(t *testing.T) {
	got := renderPlan(restore.Plan{Actions: []restore.Action{
		{Kind: "dead", Args: []string{"=default:0.0", "default_0_0", "exit 3"}},
		{Kind: "dead", Args: []string{"=default:0.1", "", "kill -9 $$"}},
	}})
	want := []string{
		"would restore dead pane =default:0.0 with its saved output, then `exit 3`",
		"would restore dead pane =default:0.1 (no saved output), then `kill -9 $$`",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("renderPlan =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
