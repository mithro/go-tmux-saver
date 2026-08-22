package tmuxctl

import (
	"context"
	"testing"
)

func TestFakeRecordsCallsAndReplies(t *testing.T) {
	f := &Fake{Replies: map[string][]string{"list-sessions": {"a", "b"}}}
	got, err := f.Run(context.Background(), "list-sessions")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := f.Run(context.Background(), "nope"); err == nil {
		t.Fatal("unknown command should error")
	}
	if len(f.Calls) != 2 || f.Calls[0] != "list-sessions" {
		t.Fatalf("calls = %v", f.Calls)
	}
}
