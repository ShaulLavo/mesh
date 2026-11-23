package oneshot

import (
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Close reasons, the same words pipe jobs report.
const (
	CloseEOF          = "eof"
	CloseDrainTimeout = "drain_timeout"
	CloseReadError    = "read_error"
)

// The drain after the leader exits matches pipe workers: a pipe quiet for
// drainSilence is cut off, and the whole drain ends after drainTotal, so a
// descendant holding stdout open cannot hold the reply.
var (
	drainSilence = 250 * time.Millisecond
	drainTotal   = 2 * time.Second
)

// Stream is one output stream: the first bytes up to the run's limit, and
// how many bytes the command wrote in all while the daemon was reading.
type Stream struct {
	Bytes       []byte
	Total       int64
	Truncated   bool
	CloseReason string
}

// epoch anchors the monotonic offsets the drain compares.
var epoch = time.Now()

func monotonic() int64 { return int64(time.Since(epoch)) }

type stream struct {
	file     *os.File
	limit    int
	kept     []byte
	total    int64
	reason   string
	lastRead atomic.Int64
	done     chan struct{}
	cutOnce  sync.Once
}

func newStream(file *os.File, limit int) *stream {
	s := &stream{file: file, limit: limit, done: make(chan struct{})}
	s.lastRead.Store(monotonic())
	return s
}

// read keeps the first limit bytes and keeps reading past them, so the
// command never blocks on a full pipe.
func (s *stream) read() {
	defer close(s.done)
	buf := make([]byte, 32<<10)
	for {
		n, err := s.file.Read(buf)
		if n > 0 {
			s.keep(buf[:n])
			s.lastRead.Store(monotonic())
		}
		if err == nil {
			continue
		}
		s.reason = closeReason(err)
		return
	}
}

func (s *stream) keep(p []byte) {
	s.total += int64(len(p))
	room := s.limit - len(s.kept)
	if room <= 0 {
		return
	}
	s.kept = append(s.kept, p[:min(room, len(p))]...)
}

func closeReason(err error) string {
	switch {
	case errors.Is(err, io.EOF):
		return CloseEOF
	case errors.Is(err, os.ErrClosed):
		return CloseDrainTimeout
	default:
		return CloseReadError
	}
}

// cut closes the reader's descriptor. A reader that already reached EOF
// keeps its reason.
func (s *stream) cut() { s.cutOnce.Do(func() { _ = s.file.Close() }) }

func (s *stream) result() Stream {
	return Stream{Bytes: s.kept, Total: s.total, Truncated: s.total > int64(len(s.kept)), CloseReason: s.reason}
}

// silentFor counts quiet time from no earlier than the drain's start.
func (s *stream) silentFor(start int64) time.Duration {
	return time.Duration(monotonic() - max(s.lastRead.Load(), start))
}

// drain waits for every stream at once, each on its own silence window, and
// stops early when the run is cancelled.
func drain(streams []*stream, stop <-chan struct{}, gate *cutGate) {
	hard := make(chan struct{})
	timer := time.AfterFunc(drainTotal, func() { close(hard) })
	defer timer.Stop()
	start := monotonic()
	var group sync.WaitGroup
	for _, s := range streams {
		group.Go(func() { s.drain(hard, stop, start, gate) })
	}
	group.Wait()
}

// drain cuts the stream off unless it ends by itself first; the gate holds
// the cut while an escalation is in progress.
func (s *stream) drain(hard, stop <-chan struct{}, start int64, gate *cutGate) {
	if !s.awaitQuiet(hard, stop, start) {
		gate.cut(s)
	}
}

// cutGate gives the cleanup loop the pipes during an escalation: from the
// first signal until SIGKILL, a drain that would cut a pipe waits, so a
// member running its signal handler keeps its output.
type cutGate struct {
	mu        sync.Mutex
	escalated bool
	killed    chan struct{}
	killOnce  sync.Once
}

func newCutGate() *cutGate { return &cutGate{killed: make(chan struct{})} }

// cut closes the stream now, or, during an escalation, once SIGKILL went
// out or the stream ended by itself. The check and the cut share the lock,
// so an escalation never starts between them.
func (g *cutGate) cut(s *stream) {
	g.mu.Lock()
	escalated := g.escalated
	if !escalated {
		s.cut()
	}
	g.mu.Unlock()
	if !escalated {
		return
	}
	select {
	case <-g.killed:
	case <-s.done:
	}
	s.cut()
}

func (g *cutGate) escalate() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.escalated = true
}

func (g *cutGate) kill() { g.killOnce.Do(func() { close(g.killed) }) }

// awaitQuiet reports whether the stream ended by itself before it was
// silent for drainSilence, the total drain ended, or the run was cancelled.
func (s *stream) awaitQuiet(hard, stop <-chan struct{}, start int64) bool {
	for {
		wait := drainSilence - s.silentFor(start)
		if wait <= 0 {
			return ended(s.done)
		}
		timer := time.NewTimer(wait)
		select {
		case <-s.done:
			timer.Stop()
			return true
		case <-hard:
			timer.Stop()
			return ended(s.done)
		case <-stop:
			timer.Stop()
			return ended(s.done)
		case <-timer.C:
		}
	}
}

func ended(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}
