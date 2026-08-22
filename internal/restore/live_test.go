package restore

import (
	"context"
	"testing"

	"github.com/mithro/go-tmux-saver/internal/tmuxctl"
)

// TestQueryLive covers FINDING 2: QueryLive had no tests. It exercises the
// list-windows reply's grouped-clone exclusion, right-to-left parsing of a
// window name containing embedded tabs, and the list-clients-based Clients
// count.
//
// QueryLive's Clients contract: it is the number of non-empty lines
// returned by `list-clients -F "#{client_name}"` — i.e. the number of
// attached tmux clients, one per line.
func TestQueryLive(t *testing.T) {
	fake := &tmuxctl.Fake{Replies: map[string][]string{
		liveWinCmd: {
			"default\t0\th\t0",
			"default-1\t0\th\t1",         // grouped clone (session_grouped=1): must be excluded
			"net\t3\tname\twith\ttab\t0", // window name itself contains tabs; parsed right-to-left
		},
		liveClientsCmd: {"/dev/pts/1", "/dev/pts/2"},
	}}

	live, err := QueryLive(context.Background(), fake)
	if err != nil {
		t.Fatalf("QueryLive: %v", err)
	}

	if len(live.Sessions) != 2 {
		t.Fatalf("expected exactly 2 sessions (default, net), got %d: %+v", len(live.Sessions), live.Sessions)
	}
	if got, want := live.Sessions["default"], []LiveWindow{{Index: 0, Name: "h"}}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("default windows = %+v, want %+v", got, want)
	}
	if got, want := live.Sessions["net"], []LiveWindow{{Index: 3, Name: "name\twith\ttab"}}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("net windows = %+v, want %+v", got, want)
	}
	if live.Clients != 2 {
		t.Errorf("Clients = %d, want 2", live.Clients)
	}
	if IsSeedOnly(live, "default", "h") {
		t.Error("must not be seed-only: the net session also exists")
	}
}

func TestQueryLiveSeedOnly(t *testing.T) {
	fake := &tmuxctl.Fake{Replies: map[string][]string{
		liveWinCmd:     {"default\t0\th\t0"},
		liveClientsCmd: {},
	}}

	live, err := QueryLive(context.Background(), fake)
	if err != nil {
		t.Fatalf("QueryLive: %v", err)
	}
	if !IsSeedOnly(live, "default", "h") {
		t.Errorf("should be seed-only: %+v", live)
	}
}

func TestQueryLiveMalformedLine(t *testing.T) {
	fake := &tmuxctl.Fake{Replies: map[string][]string{
		liveWinCmd: {"default\t0"}, // too few fields: no window_index/window_name
	}}
	if _, err := QueryLive(context.Background(), fake); err == nil {
		t.Fatal("expected an error for a malformed list-windows line")
	}
}
