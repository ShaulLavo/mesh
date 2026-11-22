package execution

import (
	"context"
	"testing"
	"time"
)

func TestPipeOfferExactStopsWhileQueueIsFull(t *testing.T) {
	reader, stopReader := context.WithCancel(t.Context())
	defer stopReader()
	// Leave the writer stalled so the queue cannot make space during the test.
	c := &Capture{
		store:    &streamStore{binding: Binding{Kind: KindPipe}},
		writeCtx: t.Context(), wake: make(chan struct{}, 1),
		space: make(chan struct{}, 1), done: make(chan struct{}),
	}
	if !c.OfferExact(reader, make([]byte, captureQueue)) {
		t.Fatal("first capture batch was not admitted")
	}
	<-c.wake
	result := make(chan bool, 1)
	go func() { result <- c.OfferExact(reader, []byte("at cutoff")) }()
	select {
	case <-c.wake:
	case <-time.After(10 * time.Second):
		t.Fatal("capture did not reach its full queue")
	}
	stopReader()
	select {
	case accepted := <-result:
		if accepted || c.size != captureQueue {
			t.Fatalf("cutoff admitted waiter's bytes: accepted=%v size=%d", accepted, c.size)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reader remained blocked on capture space after cutoff")
	}
}
