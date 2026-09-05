package sshd

import (
	"container/list"
	"fmt"
	"log"
	"net"
	"runtime/debug"
	"sync"
	"time"

	charmssh "github.com/charmbracelet/ssh"
	"golang.org/x/time/rate"
)

const maximumSSHRateEntries = 1024

type sshRateEntry struct {
	address string
	limiter *rate.Limiter
}

type sshRateLimiter struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
}

// Keep terminal discovery out of daemon and hook startup. Wish's middleware
// imports Bubble Tea v1, which queries a controlling terminal during package init.
func secureMiddleware(handler charmssh.Handler) charmssh.Handler {
	limiter := &sshRateLimiter{entries: make(map[string]*list.Element)}
	return func(current charmssh.Session) {
		started := time.Now()
		defer finishSSHSessionLog(current, started)
		pty, _, _ := current.Pty()
		log.Printf("sshd: connect user=%q remote=%s command=%q term=%q size=%dx%d", current.User(), current.RemoteAddr(), current.RawCommand(), pty.Term, pty.Window.Width, pty.Window.Height)
		if !limiter.allow(current.RemoteAddr()) {
			_, _ = fmt.Fprintln(current.Stderr(), "rate limit exceeded, please try again later")
			_ = current.Exit(1)
			_ = current.Close()
			return
		}
		handler(current)
	}
}

func finishSSHSessionLog(current charmssh.Session, started time.Time) {
	if failure := recover(); failure != nil {
		log.Printf("sshd: session panic: %v\n%s", failure, debug.Stack())
		_ = current.Exit(1)
		_ = current.Close()
	}
	log.Printf("sshd: disconnect remote=%s duration=%s", current.RemoteAddr(), time.Since(started))
}

func (limiter *sshRateLimiter) allow(remote net.Addr) bool {
	address := remote.String()
	if tcp, ok := remote.(*net.TCPAddr); ok {
		address = tcp.IP.String()
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if existing := limiter.entries[address]; existing != nil {
		limiter.order.MoveToFront(existing)
		return existing.Value.(sshRateEntry).limiter.Allow()
	}
	entry := sshRateEntry{address: address, limiter: rate.NewLimiter(rate.Every(100*time.Millisecond), 20)}
	limiter.entries[address] = limiter.order.PushFront(entry)
	if limiter.order.Len() > maximumSSHRateEntries {
		oldest := limiter.order.Back()
		delete(limiter.entries, oldest.Value.(sshRateEntry).address)
		limiter.order.Remove(oldest)
	}
	return entry.limiter.Allow()
}
