package recovery

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type checkpointWrite struct {
	record  Record
	waiters []chan error
}

// Writer keeps one pending checkpoint. A slow disk never queues stale screens.
type Writer struct {
	mu      sync.Mutex
	pending *checkpointWrite
	closed  bool
	wake    chan struct{}
	done    chan struct{}
	write   func(Record) error
	report  func(error)
}

func NewWriter(dir string, report func(error)) *Writer {
	return newWriter(func(record Record) error { return Write(dir, record) }, report)
}

func newWriter(write func(Record) error, report func(error)) *Writer {
	w := &Writer{wake: make(chan struct{}, 1), done: make(chan struct{}), write: write, report: report}
	go w.run()
	return w
}

// Submit transfers ownership of record and acknowledges its durable write.
// When a newer snapshot replaces it, that newer write acknowledges both.
func (w *Writer) Submit(record Record) <-chan error {
	ack := make(chan error, 1)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		ack <- fmt.Errorf("recovery: checkpoint writer is closed")
		return ack
	}
	if w.pending == nil {
		w.pending = &checkpointWrite{}
	}
	if len(w.pending.waiters) >= 256 {
		ack <- fmt.Errorf("recovery: too many pending checkpoint updates")
		return ack
	}
	w.pending.record = record
	w.pending.waiters = append(w.pending.waiters, ack)
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return ack
}

func (w *Writer) Close() {
	w.mu.Lock()
	w.closed = true
	select {
	case w.wake <- struct{}{}:
	default:
	}
	w.mu.Unlock()
	<-w.done
}

func (w *Writer) take() (*checkpointWrite, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	pending := w.pending
	w.pending = nil
	return pending, w.closed
}

func (w *Writer) run() {
	defer close(w.done)
	var previous [sha256.Size]byte
	for range w.wake {
		pending, closed := w.take()
		if pending != nil {
			previous = w.save(pending, previous)
		}
		if closed {
			return
		}
	}
}

func (w *Writer) save(pending *checkpointWrite, previous [sha256.Size]byte) [sha256.Size]byte {
	content := pending.record
	content.CheckpointAt = time.Time{}
	encoded, err := json.Marshal(content)
	digest := sha256.Sum256(encoded)
	if err == nil && digest != previous {
		err = w.write(pending.record)
	}
	if err != nil && w.report != nil {
		w.report(err)
	}
	for _, waiter := range pending.waiters {
		waiter <- err
	}
	if err != nil {
		return previous
	}
	return digest
}
