package tmuxctl

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// BenchmarkParseReplies measures the control-mode reader's throughput on a
// capture-pane-shaped reply (one big %begin/%end block of long lines). It
// exists to keep the transport honest: a save's wall time is dominated by
// what the tmux server spends building the reply, and this benchmark is the
// evidence that the parsing side is not what costs.
func BenchmarkParseReplies(b *testing.B) {
	const lines = 100000
	line := strings.Repeat("x", 140)
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%%begin 1 1 0\n")
	for i := 0; i < lines; i++ {
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	fmt.Fprintf(&buf, "%%end 1 1 0\n")
	blob := buf.Bytes()
	b.SetBytes(int64(len(blob)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := make(chan Reply, 1)
		done := make(chan struct{})
		go func() {
			for range out {
			}
			close(done)
		}()
		if err := ParseReplies(bytes.NewReader(blob), out); err != nil {
			b.Fatalf("parse: %v", err)
		}
		close(out)
		<-done
	}
}
