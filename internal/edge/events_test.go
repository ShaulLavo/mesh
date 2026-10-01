package edge

import (
	"io"
	"log"
	"sync"
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
	for writer.writes.Load() < maximumEventsPerWindow+1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := writer.writes.Load(); got != maximumEventsPerWindow+1 {
		t.Fatalf("sink writes = %d, want bounded %d", got, maximumEventsPerWindow+1)
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

func TestEventQueueOverflowReportsExactDropsAndPreservesOtherCategory(t *testing.T) {
	writer := &blockingEventWriter{started: make(chan struct{}), release: make(chan struct{})}
	var clockMu sync.Mutex
	now := time.Now()
	capture := &eventCapture{}
	logger := newEventLogger(log.New(io.MultiWriter(writer, capture), "", 0), func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now })
	defer logger.Close()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(writer.release) }) }
	defer release()
	logger.Print("edge event=invalid-public-host")
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("sink did not block")
	}
	for window := 0; window < 2; window++ {
		clockMu.Lock()
		now = now.Add(eventWindow)
		clockMu.Unlock()
		for i := 0; i < maximumEventsPerWindow; i++ {
			logger.Print("edge event=invalid-public-host")
		}
	}
	logger.Print("edge event=origin-unavailable origin=important")
	clockMu.Lock()
	now = now.Add(eventWindow)
	clockMu.Unlock()
	logger.rotate(false)
	release()
	deadline := time.Now().Add(time.Second)
	for (!capture.contains("category=invalid-public-host dropped=56") || !capture.contains("origin=important")) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !capture.contains("category=invalid-public-host dropped=56") {
		t.Fatal("queue overflow lost its exact drop count")
	}
	if !capture.contains("origin=important") {
		t.Fatal("full host queue hid origin failure")
	}
}
