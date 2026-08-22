package trace

import (
	"os"
	"testing"
)

// TestDisabledByDefault guards the property the rest of the code relies on:
// with GTS_TRACE unset, tracing is off and the helpers are safe, silent
// no-ops (they must never write to stderr during a normal save).
func TestDisabledByDefault(t *testing.T) {
	if _, set := os.LookupEnv("GTS_TRACE"); set {
		t.Skip("GTS_TRACE is set in this environment")
	}
	if Enabled {
		t.Fatal("Enabled = true with GTS_TRACE unset")
	}
	Logf("this must not be printed: %d", 1)
	stop := Time("phase")
	if stop == nil {
		t.Fatal("Time returned a nil stop func")
	}
	stop()
}

// TestEnabledWrites checks the enabled path actually formats and emits,
// by driving the same code with Enabled forced on.
func TestEnabledWrites(t *testing.T) {
	old := Enabled
	Enabled = true
	defer func() { Enabled = old }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldErr := os.Stderr
	os.Stderr = w
	Logf("hello %s", "world")
	Time("phase")()
	os.Stderr = oldErr
	w.Close()

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	got := string(buf[:n])
	if want := "trace: hello world\n"; len(got) < len(want) || got[:len(want)] != want {
		t.Fatalf("stderr = %q, want it to start with %q", got, want)
	}
}
