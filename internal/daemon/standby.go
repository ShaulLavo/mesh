package daemon

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/worker"
)

// standbyPool keeps one pre-started worker for the next keyed launch, so the
// launch does not pay for starting and initializing a Go process. A standby
// holds nothing: closing its pipe, or this daemon exiting, ends it. A launch
// reserves the standby's session ID when it is free and takes the standby
// that matches; any other launch starts a fresh worker as before.
type standbyPool struct {
	sessionsDir string
	executable  string
	env         func() []string
	start       func(sessionsDir, executable string, env []string) (*worker.Standby, error)

	mu       sync.Mutex
	current  *worker.Standby
	starting bool
	// active is set once the daemon has finished starting; a lifecycle that
	// never activates its pool (tests with their own launcher) starts none.
	active bool
	closed bool
}

// activate starts the first standby; until then refill does nothing.
func (p *standbyPool) activate() {
	p.mu.Lock()
	p.active = true
	p.mu.Unlock()
	p.refill()
}

func newStandbyPool(sessionsDir, executable string, env func() []string) *standbyPool {
	return &standbyPool{sessionsDir: sessionsDir, executable: executable, env: env, start: worker.StartStandby}
}

// preferredID is the session ID the next reservation should try first.
// A standby whose session directory already exists lost its ID to a
// reservation that never launched it, so it is dropped instead.
func (p *standbyPool) preferredID() string {
	p.mu.Lock()
	s := p.current
	p.mu.Unlock()
	if s == nil {
		return ""
	}
	if _, err := os.Lstat(s.Dir); !errors.Is(err, os.ErrNotExist) {
		p.drop(s.ID)
		return ""
	}
	return s.ID
}

// take returns the standby started under id, which a reservation now owns.
// Nothing else can use it afterwards.
func (p *standbyPool) take(id string) *worker.Standby {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil || p.current.ID != id {
		return nil
	}
	s := p.current
	p.current = nil
	return s
}

// standbyRefillDelay keeps a new standby's start-up off the CPU while the job
// that used the last one finishes; on a two-core host it slowed that job's
// completion by about 20 ms.
const standbyRefillDelay = 100 * time.Millisecond

// refillSoon starts the next standby once the job that took the last one has
// had time to finish.
func (p *standbyPool) refillSoon() { time.AfterFunc(standbyRefillDelay, p.refill) }

// refill starts a standby when none is waiting. A standby that dies on its
// own is dropped and replaced only at the next refill, so a binary that
// cannot start one is not restarted in a loop.
func (p *standbyPool) refill() {
	p.mu.Lock()
	if !p.active || p.closed || p.starting || p.current != nil || p.executable == "" {
		p.mu.Unlock()
		return
	}
	p.starting = true
	p.mu.Unlock()
	s, err := p.start(p.sessionsDir, p.executable, p.env())
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starting = false
	if err != nil {
		return
	}
	if p.closed {
		s.Discard()
		return
	}
	p.current = s
	go p.forgetWhenExited(s)
}

func (p *standbyPool) forgetWhenExited(s *worker.Standby) {
	<-s.Exited()
	s.Discard()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == s {
		p.current = nil
	}
}

// drop discards the waiting standby if it carries id: a reservation took
// that ID and never launched, or a reservation that wanted it got another,
// so the standby can no longer serve anyone. The next one starts soon.
func (p *standbyPool) drop(id string) {
	p.mu.Lock()
	s := p.current
	if s == nil || s.ID != id {
		p.mu.Unlock()
		return
	}
	p.current = nil
	p.mu.Unlock()
	s.Discard()
	p.refillSoon()
}

// reserved reports a reservation that asked for preferred and got id.
func (p *standbyPool) reserved(preferred, id string) {
	if preferred != "" && preferred != id {
		p.drop(preferred)
	}
}

// close ends the waiting standby; later refills do nothing.
func (p *standbyPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.current != nil {
		p.current.Discard()
		p.current = nil
	}
}

func standbyExecutable(configured string) string {
	if configured != "" {
		return configured
	}
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
}
