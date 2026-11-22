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
	mu       sync.Mutex
	events   []mcpwire.Event
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

// push queues an event. A terminal event is always accepted: it is one
// message of at most the single-message cap, and dropping it would turn a
// finished request into an unknown one.
func (q *eventQueue) push(event mcpwire.Event) bool {
	size := len(event.Message.Raw())
	q.mu.Lock()
	defer q.mu.Unlock()
	if !event.Terminal && (len(q.events) >= httpCallQueueEvents || q.bytes+size > httpCallQueueBytes) {
		q.slow, q.finished = true, true
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
	case len(q.events) > 0:
		event, q.events = q.events[0], q.events[1:]
		q.bytes -= len(event.Message.Raw())
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
		if !q.push(event) {
			// Stop the server's writes now; the sink may take a long time to return.
			stream.close()
			return
		}
		if event.Terminal {
			return
		}
	}
}
