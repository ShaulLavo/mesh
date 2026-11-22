package mcphost

import (
	"context"
	"fmt"
	"sync"

	"github.com/shaul/mesh/internal/mcpwire"
)

// A call's reader runs ahead of its sink by at most this much. Beyond it the
// call fails with ErrSlowConsumer, so a stuck caller cannot hold the hosted
// server's response open or take memory from other calls.
const (
	httpCallQueueEvents = 1024
	httpCallQueueBytes  = 8 << 20
)

// eventQueue hands validated events from the goroutine that reads the HTTP
// response to the goroutine that runs the sink.
type eventQueue struct {
	mu     sync.Mutex
	events []mcpwire.Event
	head   int
	// bytes counts the queued events and the one the sink is handling.
	bytes    int
	finished bool
	failure  error
	slow     bool
	mismatch bool
	wake     chan struct{}
}

func newEventQueue() *eventQueue { return &eventQueue{wake: make(chan struct{}, 1)} }

func (q *eventQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// push queues an event within the call's budget. Any event that does not fit,
// the terminal result included, ends the call as a slow consumer: the error is
// small and needs no queue space, and queued payloads are dropped at once.
func (q *eventQueue) push(event mcpwire.Event) bool {
	size := len(event.Message.Raw())
	q.mu.Lock()
	defer q.mu.Unlock()
	queued := len(q.events) - q.head
	if q.bytes+size > httpCallQueueBytes || (!event.Terminal && queued >= httpCallQueueEvents) {
		q.slow, q.finished = true, true
		q.events, q.head = nil, 0
		q.signal()
		return false
	}
	q.events = append(q.events, event)
	q.bytes += size
	if event.Terminal {
		q.finished = true
	}
	q.signal()
	return true
}

// release returns a delivered event's bytes to the budget once the sink is done with it.
func (q *eventQueue) release(event mcpwire.Event) {
	q.mu.Lock()
	q.bytes -= len(event.Message.Raw())
	q.mu.Unlock()
}

// fail ends the queue with the reader's error.
func (q *eventQueue) fail(err error) {
	q.mu.Lock()
	q.finished, q.failure = true, err
	q.mu.Unlock()
	q.signal()
}

// rejectHeaders ends the queue because the upstream refused the mirrored
// parameter headers; the terminal error is not for the caller.
func (q *eventQueue) rejectHeaders() {
	q.mu.Lock()
	q.finished, q.mismatch = true, true
	q.mu.Unlock()
	q.signal()
}

// take returns the next event, or the call's final error.
func (q *eventQueue) take(ctx context.Context) (mcpwire.Event, error) {
	for {
		event, ready, err := q.poll()
		if ready {
			return event, err
		}
		select {
		case <-q.wake:
		case <-ctx.Done():
			return mcpwire.Event{}, fmt.Errorf("MCP upstream call: %w", ctx.Err())
		}
	}
}

func (q *eventQueue) poll() (event mcpwire.Event, ready bool, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case q.slow:
		return mcpwire.Event{}, true, ErrSlowConsumer
	case q.head < len(q.events):
		event = q.events[q.head]
		// The slot must not keep a delivered payload alive.
		q.events[q.head] = mcpwire.Event{}
		q.head++
		if q.head == len(q.events) {
			q.events, q.head = q.events[:0], 0
		}
		return event, true, nil
	case q.mismatch:
		return mcpwire.Event{}, true, ErrHeaderMismatch
	case q.finished && q.failure != nil:
		return mcpwire.Event{}, true, q.failure
	case q.finished:
		return mcpwire.Event{}, true, mcpwire.ErrInterrupted
	}
	return mcpwire.Event{}, false, nil
}

// pump reads the response into the queue until the terminal event, an error,
// or an overflow. It stops at the terminal event: the stream is closed right
// after delivery, so a frame sent later is never read.
func pump(stream *httpStream, q *eventQueue) {
	// The reader owns the connection's lifetime, so a blocked sink cannot
	// keep a finished or failed stream open.
	defer stream.close()
	for {
		event, err := stream.next()
		if err != nil {
			q.fail(err)
			return
		}
		if event.Terminal && stream.refresh {
			q.rejectHeaders()
			return
		}
		if !q.push(event) || event.Terminal {
			return
		}
	}
}
