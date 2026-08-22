// Package trace emits optional per-phase timings to stderr.
//
// It is inert unless the GTS_TRACE environment variable is set to a non-empty
// value, and is meant for diagnosing where a save's wall time goes (Dial, the
// list-* commands, per-pane capture-pane, the /proc scan, staging) without
// changing normal output. Enabled is read once at process start so the hot
// paths cost one bool test.
package trace

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Enabled reports whether tracing was requested via GTS_TRACE.
var Enabled = os.Getenv("GTS_TRACE") != ""

var mu sync.Mutex

// Logf writes one trace line to stderr. It is a no-op when tracing is off.
func Logf(format string, args ...any) {
	if !Enabled {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintf(os.Stderr, "trace: "+format+"\n", args...)
}

// Time returns a function that, when called, logs the elapsed time under
// name. Use it as `defer trace.Time("phase")()`. It is a no-op when tracing
// is off.
func Time(name string) func() {
	if !Enabled {
		return func() {}
	}
	start := time.Now()
	return func() { Logf("%-22s %v", name, time.Since(start)) }
}
