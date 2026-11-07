package cli

import (
	"context"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

func TestPickerCacheWriterCoalescesAndJoins(t *testing.T) {
	entered := make(chan string, 2)
	firstRelease := make(chan struct{})
	finished := make(chan struct{})
	queue, stop := startPickerCacheWriter(t.Context(), func(ctx context.Context, view StateView) {
		id := view.Sessions[0].ID
		entered <- id
		if id == "FIRST" {
			waitPickerFirstWrite(ctx, firstRelease)
			return
		}
		<-ctx.Done()
		close(finished)
	})
	t.Cleanup(stop)
	queue(StateView{Sessions: []protocol.SessionInfo{{ID: "FIRST"}}})
	if got := awaitPickerWrite(t, entered); got != "FIRST" {
		t.Fatalf("first write = %s", got)
	}
	offered := make(chan struct{})
	go func() {
		queue(StateView{Sessions: []protocol.SessionInfo{{ID: "OLD"}}})
		queue(StateView{Sessions: []protocol.SessionInfo{{ID: "LATEST"}}})
		close(offered)
	}()
	select {
	case <-offered:
	case <-time.After(2 * time.Second):
		t.Fatal("live publication waited for cache admission")
	}
	close(firstRelease)
	if got := awaitPickerWrite(t, entered); got != "LATEST" {
		t.Fatalf("pending write = %s, want latest", got)
	}
	stop()
	select {
	case <-finished:
	default:
		t.Fatal("stop returned before the owned cache write joined")
	}
}

func waitPickerFirstWrite(ctx context.Context, release <-chan struct{}) {
	select {
	case <-release:
	case <-ctx.Done():
	}
}

func awaitPickerWrite(t *testing.T, entered <-chan string) string {
	t.Helper()
	select {
	case id := <-entered:
		return id
	case <-time.After(2 * time.Second):
		t.Fatal("owned cache writer did not enter")
	}
	return ""
}
