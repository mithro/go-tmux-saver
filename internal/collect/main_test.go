package collect

import (
	"os"
	"testing"

	"github.com/mithro/go-tmux-saver/internal/tmuxtest"
)

// TestMain isolates the tmux socket directory for this package's tests so a
// throwaway server can never collide with — or be mistaken for — a
// production tmux server on the same host. See internal/tmuxtest.Isolate.
func TestMain(m *testing.M) {
	os.Exit(tmuxtest.Isolate(m.Run))
}
