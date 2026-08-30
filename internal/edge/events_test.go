package edge

import (
	"log"
	"sync/atomic"
	"testing"
	"time"
)

func TestEventLoggerNeverBlocksPublicRequestsAndBoundsOutput(t *testing.T) {
	writer := &blockingEventWriter{started: make(chan struct{}), release: make(chan struct{})}
	logger := newEventLogger(log.New(writer, "", 0), func() time.Time {
		return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	})
	defer logger.Close()
	release := func() {
		select {
		case <-writer.release:
		default:
			close(writer.release)
		}
	}
	defer release()
	logger.Print("first")
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("event sink did not receive the first event")
	}

	done := make(chan struct{})
	go func() {
		for index := 0; index < 10_000; index++ {
			logger.Printf("edge event=invalid-request index=%d", index)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("public event emission blocked on the operator sink")
	}
	release()
	deadline := time.Now().Add(time.Second)
	for writer.writes.Load() < maximumEventsPerWindow && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := writer.writes.Load(); got != maximumEventsPerWindow {
		t.Fatalf("sink writes = %d, want bounded %d", got, maximumEventsPerWindow)
	}
}

type blockingEventWriter struct {
	started chan struct{}
	release chan struct{}
	writes  atomic.Int64
}

func (w *blockingEventWriter) Write(contents []byte) (int, error) {
	if w.writes.Add(1) == 1 {
		close(w.started)
	}
	<-w.release
	return len(contents), nil
}
