package transport

import "sync"

// Input writers must leave the queue when their connection retires, so a
// terminal reset can wait for its reader without waiting for reconnection.
type connectionGate struct {
	once sync.Once
	held chan struct{}
}

func (g *connectionGate) init() { g.once.Do(func() { g.held = make(chan struct{}, 1) }) }

func (g *connectionGate) Lock() { g.LockUntil(nil) }

func (g *connectionGate) LockUntil(done <-chan struct{}) bool {
	g.init()
	select {
	case <-done:
		return false
	case g.held <- struct{}{}:
		return true
	}
}

func (g *connectionGate) TryLock() bool {
	g.init()
	select {
	case g.held <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *connectionGate) Unlock() { <-g.held }
