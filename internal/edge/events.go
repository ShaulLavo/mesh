package edge

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

const (
	maximumQueuedEvents    = 64
	maximumEventsPerWindow = 60
	eventWindow            = time.Minute
)

// eventLogger bounds request-driven diagnostics before they reach an
// operator-supplied sink. Public traffic never waits for that sink.
type eventLogger struct {
	sink   *log.Logger
	now    func() time.Time
	queue  chan string
	cancel context.CancelFunc

	mu          sync.Mutex
	windowStart time.Time
	count       int
}

func newEventLogger(sink *log.Logger, now func() time.Time) *eventLogger {
	logger := &eventLogger{sink: sink, now: now}
	if sink == nil {
		return logger
	}
	logger.queue = make(chan string, maximumQueuedEvents)
	ctx, cancel := context.WithCancel(context.Background())
	logger.cancel = cancel
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-logger.queue:
				sink.Print(message)
			}
		}
	}()
	return logger
}

func (l *eventLogger) Print(values ...any) { l.emit(fmt.Sprint(values...)) }

func (l *eventLogger) Printf(format string, values ...any) {
	l.emit(fmt.Sprintf(format, values...))
}

func (l *eventLogger) emit(message string) {
	if l == nil || l.sink == nil {
		return
	}
	now := l.now().UTC()
	l.mu.Lock()
	if l.windowStart.IsZero() || now.Sub(l.windowStart) >= eventWindow {
		l.windowStart = now
		l.count = 0
	}
	if l.count >= maximumEventsPerWindow {
		l.mu.Unlock()
		return
	}
	l.count++
	l.mu.Unlock()
	select {
	case l.queue <- message:
	default:
	}
}

func (l *eventLogger) Close() {
	if l != nil && l.cancel != nil {
		l.cancel()
	}
}
