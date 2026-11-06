package cli

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// The watcher remains alive through fixture cleanups, which can consume time
// outside the cancellation assertions. It observes only this subtest's phases.
type cancellationPhaseTrace struct {
	t        *testing.T
	started  time.Time
	mu       sync.Mutex
	stage    string
	since    time.Time
	stopped  bool
	changed  chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func newCancellationPhaseTrace(t *testing.T) *cancellationPhaseTrace {
	t.Helper()
	trace := &cancellationPhaseTrace{
		t: t, started: time.Now(),
		changed: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	// Registered first so temporary directories and environment restoration run
	// before the watcher is stopped and joined, even when setup calls Fatal.
	t.Cleanup(trace.close)
	trace.phase("host/catalog setup")
	go trace.watch()
	return trace
}

func (p *cancellationPhaseTrace) phase(stage string) {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	now := time.Now()
	if !p.since.IsZero() {
		p.log(now, "phase finished")
	}
	p.stage, p.since = stage, now
	p.log(now, "phase started")
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

func (p *cancellationPhaseTrace) event(event string) {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stopped {
		p.log(time.Now(), event)
	}
}

func (p *cancellationPhaseTrace) log(now time.Time, event string) {
	p.t.Helper()
	p.t.Logf("cancellation trace: timestamp=%s elapsed=%s stage=%q phase_elapsed=%s event=%q",
		now.UTC().Format(time.RFC3339Nano), now.Sub(p.started), p.stage, now.Sub(p.since), event)
}

func (p *cancellationPhaseTrace) watch() {
	defer close(p.done)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for p.wait(timer) {
	}
}

func (p *cancellationPhaseTrace) wait(timer *time.Timer) bool {
	select {
	case <-p.stop:
		return false
	case <-p.changed:
		timer.Reset(p.remaining())
	case <-timer.C:
		if p.snapshotIfLong() {
			return false
		}
		timer.Reset(p.remaining())
	}
	return true
}

func (p *cancellationPhaseTrace) remaining() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return max(0, time.Second-time.Since(p.since))
}

func (p *cancellationPhaseTrace) snapshotIfLong() bool {
	p.mu.Lock()
	now, stage, since := time.Now(), p.stage, p.since
	p.mu.Unlock()
	if now.Sub(since) < time.Second {
		return false
	}
	// One fixed-size buffer and at most one snapshot per subtest. Capture all
	// runtime goroutines so a stalled phase can be related to its relay or timer.
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	p.t.Logf("cancellation trace: timestamp=%s elapsed=%s stage=%q phase_elapsed=%s event=%q bytes=%d truncated=%t capture_elapsed=%s\n%s",
		now.UTC().Format(time.RFC3339Nano), now.Sub(p.started), stage, now.Sub(since), "long phase goroutine snapshot",
		n, n == len(buf), time.Since(now), buf[:n])
	return true
}

func (p *cancellationPhaseTrace) close() {
	p.stopOnce.Do(func() {
		close(p.stop)
		<-p.done
		p.mu.Lock()
		defer p.mu.Unlock()
		p.log(time.Now(), "fixture cleanup complete; watcher joined")
		p.stopped = true
	})
}
