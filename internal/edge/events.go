package edge

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"
)

const (
	maximumQueuedEvents    = 64
	maximumEventsPerWindow = 60
	eventWindow            = time.Minute
)

// Categories are code-owned: request strings cannot allocate new log budgets.
var eventCategories = [...]string{"invalid-request", "invalid-public-host", "invalid-client-address", "untrusted-front-door", "plaintext-direct-request", "invalid-server-name", "invalid-forwarded-metadata", "rate-limit", "reserved-terminal-path", "unknown-route", "origin-unavailable", "cookies-stripped", "other"}

type eventBudget struct {
	queue   chan string
	start   time.Time
	count   int
	dropped uint64
	report  uint64
}

// Each category has its own bounded queue, so a blocked sink and a host flood
// cannot consume the slots reserved for origin failures.
type eventLogger struct {
	sink    *log.Logger
	now     func() time.Time
	notify  chan struct{}
	cancel  context.CancelFunc
	mu      sync.Mutex
	budgets [len(eventCategories)]eventBudget
}

func newEventLogger(sink *log.Logger, now func() time.Time) *eventLogger {
	l := &eventLogger{sink: sink, now: now}
	if sink == nil {
		return l
	}
	l.notify = make(chan struct{}, 1)
	for i := range l.budgets {
		l.budgets[i].queue = make(chan string, maximumQueuedEvents)
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	go l.run(ctx)
	return l
}

func (l *eventLogger) run(ctx context.Context) {
	ticker := time.NewTicker(eventWindow)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			l.rotate(true)
			l.drain()
			return
		case <-ticker.C:
			l.rotate(false)
		case <-l.notify:
		}
		l.drain()
	}
}

func (l *eventLogger) rotate(force bool) {
	now := l.now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.budgets {
		l.budgets[i].rotate(now, force)
	}
}

func (b *eventBudget) rotate(now time.Time, force bool) {
	if !force && !b.start.IsZero() && now.Sub(b.start) < eventWindow {
		return
	}
	b.report = saturatingEvents(b.report, b.dropped)
	b.dropped = 0
	b.start = now
	b.count = 0
}

func saturatingEvents(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}

func (l *eventLogger) drain() {
	for {
		l.rotate(false)
		wrote := false
		for i := range l.budgets {
			l.mu.Lock()
			b := &l.budgets[i]
			report := b.report
			b.report = 0
			var message string
			select {
			case message = <-b.queue:
			default:
			}
			l.mu.Unlock()
			if report != 0 {
				l.sink.Printf("edge event=events-dropped category=%s dropped=%d", eventCategories[i], report)
				wrote = true
			}
			if message != "" {
				l.sink.Print(message)
				wrote = true
			}
		}
		if !wrote {
			return
		}
	}
}

func (l *eventLogger) Print(values ...any)                 { l.emit(fmt.Sprint(values...)) }
func (l *eventLogger) Printf(format string, values ...any) { l.emit(fmt.Sprintf(format, values...)) }

func (l *eventLogger) emit(message string) {
	if l == nil || l.sink == nil {
		return
	}
	index := len(eventCategories) - 1
	_, kind, ok := strings.Cut(message, "event=")
	if ok {
		kind, _, _ = strings.Cut(kind, " ")
		for i, category := range eventCategories {
			if kind == category {
				index = i
				break
			}
		}
	}
	now := l.now().UTC()
	l.mu.Lock()
	b := &l.budgets[index]
	b.rotate(now, false)
	if b.count >= maximumEventsPerWindow {
		b.dropped = saturatingEvents(b.dropped, 1)
	} else {
		b.count++
		select {
		case b.queue <- message:
		default:
			b.dropped = saturatingEvents(b.dropped, 1)
		}
	}
	l.mu.Unlock()
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

func (l *eventLogger) Close() {
	if l != nil && l.cancel != nil {
		l.cancel()
	}
}
