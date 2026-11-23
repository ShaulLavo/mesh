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
// holds nothing: closing its socket, or this daemon exiting, ends it.
//
// Ownership moves in steps. The pool offers its waiting standby's session ID;
// the reservation that gets that ID claims the standby, so no other request
// can disturb it; the launch then takes it and hands it the job. A claim that
// is never taken (the request ended before launching) is released by drop, or
// expires, and its standby is discarded.
type standbyPool struct {
	sessionsDir string
	executable  string
	env         func() []string
	start       func(sessionsDir, executable string, env []string) (*worker.Standby, error)

	mu       sync.Mutex
	current  *worker.Standby
	claimed  map[string]standbyClaim
	starting bool
	// active is set once the daemon has finished starting; a lifecycle that
	// never activates its pool (tests with their own launcher) starts none.
	active bool
	closed bool
}

type standbyClaim struct {
	standby *worker.Standby
	at      time.Time
}

// standbyClaimExpiry bounds how long a claimed standby waits for its launch.
// A launch follows its reservation within milliseconds; a request that never
// launches and is not dropped (an error path) is cleaned up here instead.
const standbyClaimExpiry = 30 * time.Second

// standbyRefillDelay keeps a new standby's start-up off the CPU while the job
// that used the last one finishes; on a two-core host it slowed that job's
// completion by about 20 ms.
const standbyRefillDelay = 100 * time.Millisecond

func newStandbyPool(sessionsDir, executable string, env func() []string) *standbyPool {
	return &standbyPool{sessionsDir: sessionsDir, executable: executable, env: env, start: worker.StartStandby, claimed: make(map[string]standbyClaim)}
}

// activate starts the first standby; until then refill does nothing.
func (p *standbyPool) activate() {
	p.mu.Lock()
	p.active = true
	p.mu.Unlock()
	p.refill()
}

// preferredID is the session ID the next reservation should try first. A
// waiting standby whose session directory already exists cannot get its ID
// (an old session holds it), so it is dropped instead.
func (p *standbyPool) preferredID() string {
	p.mu.Lock()
	s := p.current
	p.mu.Unlock()
	if s == nil {
		return ""
	}
	if _, err := os.Lstat(s.Dir); !errors.Is(err, os.ErrNotExist) {
		p.dropWaiting(s)
		return ""
	}
	return s.ID
}

// dropWaiting discards s if it is still the waiting standby, and never a
// claimed one that happens to share its ID.
func (p *standbyPool) dropWaiting(s *worker.Standby) {
	p.mu.Lock()
	owned := p.current == s
	if owned {
		p.current = nil
	}
	p.mu.Unlock()
	if owned {
		s.Discard()
		p.refillSoon()
	}
}

// reserved reports a reservation that asked for preferred and got id. Getting
// it claims the standby for that request; missing it means the standby can no
// longer serve anyone.
func (p *standbyPool) reserved(preferred, id string) {
	if preferred == "" {
		return
	}
	p.mu.Lock()
	s := p.current
	if s == nil || s.ID != preferred {
		p.mu.Unlock()
		return
	}
	p.current = nil
	if id == preferred {
		p.claimed[id] = standbyClaim{standby: s, at: time.Now()}
		s = nil
		time.AfterFunc(standbyClaimExpiry+time.Second, p.expireClaims)
	}
	p.mu.Unlock()
	if s != nil {
		s.Discard()
	}
	p.refillSoon()
}

// take returns the standby claimed for id, which the launch now owns.
func (p *standbyPool) take(id string) *worker.Standby {
	p.mu.Lock()
	defer p.mu.Unlock()
	claim, ok := p.claimed[id]
	if !ok {
		return nil
	}
	delete(p.claimed, id)
	return claim.standby
}

// drop releases the claim under id: its request ended without launching,
// so the claimed standby is discarded. After a launch took it, drop does
// nothing.
func (p *standbyPool) drop(id string) {
	p.mu.Lock()
	var gone *worker.Standby
	if claim, ok := p.claimed[id]; ok {
		delete(p.claimed, id)
		gone = claim.standby
	}
	p.mu.Unlock()
	if gone == nil {
		return
	}
	gone.Discard()
	p.refillSoon()
}

// refillSoon starts the next standby once the job that took the last one has
// had time to finish.
func (p *standbyPool) refillSoon() { time.AfterFunc(standbyRefillDelay, p.refill) }

// refill starts a standby when none is waiting.
// A standby that dies on its own is replaced only at the next refill, so a
// binary that cannot start one is not restarted in a loop.
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

// expireClaims discards claims older than standbyClaimExpiry. Each claim
// schedules a call, so an idle daemon still cleans up.
func (p *standbyPool) expireClaims() {
	p.mu.Lock()
	var expired []*worker.Standby
	for id, claim := range p.claimed {
		if time.Since(claim.at) > standbyClaimExpiry {
			delete(p.claimed, id)
			expired = append(expired, claim.standby)
		}
	}
	p.mu.Unlock()
	for _, s := range expired {
		s.Discard()
	}
}

// forgetWhenExited removes a standby that exited while the pool still held
// it, and closes its socket. Once a launch took it, the launcher owns the
// socket, which may still hold the acknowledgement.
func (p *standbyPool) forgetWhenExited(s *worker.Standby) {
	<-s.Exited()
	p.mu.Lock()
	owned := p.current == s
	if owned {
		p.current = nil
	}
	if claim, ok := p.claimed[s.ID]; ok && claim.standby == s {
		delete(p.claimed, s.ID)
		owned = true
	}
	p.mu.Unlock()
	if owned {
		s.Discard()
	}
}

// close ends the waiting and claimed standbys; later refills do nothing.
func (p *standbyPool) close() {
	p.mu.Lock()
	p.closed = true
	gone := make([]*worker.Standby, 0, len(p.claimed)+1)
	if p.current != nil {
		gone, p.current = append(gone, p.current), nil
	}
	for id, claim := range p.claimed {
		delete(p.claimed, id)
		gone = append(gone, claim.standby)
	}
	p.mu.Unlock()
	for _, s := range gone {
		s.Discard()
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
