package daemon

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/tailnet"
)

const (
	maximumPublicSourceConnections = 32
	maximumPublicAuthenticating    = 32
	publicProxyHeaderTimeout       = 2 * time.Second
)

type boundedPublicListener struct {
	net.Listener
	maximum   int
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	closed    bool
	active    map[*boundedPublicConn]struct{}
	sources   map[netip.Prefix]int
	admitted  int
	proxyUIDs []uint32
	pending   chan struct{}
}

type boundedPublicConn struct {
	net.Conn
	owner     *boundedPublicListener
	once      sync.Once
	identify  sync.Once
	source    netip.Prefix
	admitted  bool
	closed    bool
	idleSince time.Time
	pending   bool
	authTimer *time.Timer
}

func newBoundedPublicListener(listener net.Listener, maximum int) *boundedPublicListener {
	return &boundedPublicListener{
		Listener: listener, maximum: maximum, done: make(chan struct{}),
		active: make(map[*boundedPublicConn]struct{}), sources: make(map[netip.Prefix]int),
		pending: make(chan struct{}, maximumPublicAuthenticating),
	}
}

func (l *boundedPublicListener) Accept() (net.Conn, error) {
	for {
		tracked, err := l.acceptTracked()
		if err != nil {
			return nil, err
		}
		if l.proxyUIDs != nil {
			return tracked, nil
		}
		tracked.RemoteAddr()
		l.mu.Lock()
		admitted := tracked.admitted
		l.mu.Unlock()
		if admitted {
			return tracked, nil
		}
	}
}

func (l *boundedPublicListener) acceptTracked() (*boundedPublicConn, error) {
	proxy := l.proxyUIDs != nil
	if proxy {
		select {
		case l.pending <- struct{}{}:
		case <-l.done:
			return nil, net.ErrClosed
		}
	}
	acceptor := l.Listener
	if proxy {
		acceptor = tailnet.ProxyListener{Listener: acceptor, AllowedUIDs: l.proxyUIDs}
	}
	connection, err := acceptor.Accept()
	if err != nil {
		if proxy {
			<-l.pending
		}
		return nil, err //nolint:wrapcheck // net/http retries Accept only when the error itself implements net.Error
	}
	tracked := &boundedPublicConn{Conn: connection, owner: l, pending: proxy}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		if proxy {
			<-l.pending
		}
		_ = connection.Close()
		return nil, net.ErrClosed
	}
	l.active[tracked] = struct{}{}
	if proxy {
		// Authentication runs in the HTTP goroutine, not the shared Accept loop.
		tracked.authTimer = time.AfterFunc(publicProxyHeaderTimeout, func() { l.expireAuthentication(tracked) })
	}
	return tracked, nil
}

func (l *boundedPublicListener) expireAuthentication(c *boundedPublicConn) {
	l.mu.Lock()
	if !c.pending {
		l.mu.Unlock()
		return
	}
	l.releaseLocked(c)
	l.mu.Unlock()
	_ = c.Close()
}

func publicConnectionSource(address net.Addr) netip.Prefix {
	peer, err := netip.ParseAddrPort(address.String())
	if err != nil {
		return netip.Prefix{}
	}
	ip := peer.Addr().Unmap().WithZone("")
	bits := 64
	if ip.Is4() {
		bits = 32
	}
	return netip.PrefixFrom(ip, bits).Masked()
}

func (c *boundedPublicConn) RemoteAddr() net.Addr {
	address := c.Conn.RemoteAddr()
	c.identify.Do(func() {
		if !c.owner.admit(c, publicConnectionSource(address)) {
			_ = c.Close()
		}
	})
	return address
}

func (c *boundedPublicConn) Read(p []byte) (int, error) {
	c.RemoteAddr()
	n, err := c.Conn.Read(p)
	if err != nil {
		return n, err //nolint:wrapcheck // net.Conn preserves timeout and EOF semantics
	}
	return n, nil
}

func (l *boundedPublicListener) admit(c *boundedPublicConn, source netip.Prefix) bool {
	l.mu.Lock()
	if l.closed || c.closed || !source.IsValid() || l.sources[source] >= maximumPublicSourceConnections {
		l.mu.Unlock()
		return false
	}
	var oldest *boundedPublicConn
	if l.admitted == l.maximum {
		for candidate := range l.active {
			if !candidate.idleSince.IsZero() && (oldest == nil || candidate.idleSince.Before(oldest.idleSince)) {
				oldest = candidate
			}
		}
		if oldest == nil {
			l.mu.Unlock()
			return false
		}
		l.releaseLocked(oldest)
	}
	l.finishAuthenticationLocked(c)
	c.source, c.admitted = source, true
	l.sources[source]++
	l.admitted++
	l.mu.Unlock()
	if oldest != nil {
		_ = oldest.Close()
	}
	return true
}

func (l *boundedPublicListener) finishAuthenticationLocked(c *boundedPublicConn) {
	if c.authTimer != nil {
		c.authTimer.Stop()
	}
	if c.pending {
		c.pending = false
		<-l.pending
	}
}

func (l *boundedPublicListener) releaseLocked(c *boundedPublicConn) {
	delete(l.active, c)
	c.closed = true
	l.finishAuthenticationLocked(c)
	if c.admitted {
		l.admitted--
		l.sources[c.source]--
		if l.sources[c.source] == 0 {
			delete(l.sources, c.source)
		}
		c.admitted = false
	}
}

func (l *boundedPublicListener) connState(connection net.Conn, state http.ConnState) {
	if secure, ok := connection.(*tls.Conn); ok {
		connection = secure.NetConn()
	}
	tracked, ok := connection.(*boundedPublicConn)
	if !ok {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if tracked.closed {
		return
	}
	tracked.idleSince = time.Time{}
	if state == http.StateIdle {
		tracked.idleSince = time.Now()
	}
}

func (l *boundedPublicListener) Close() error {
	var result error
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		close(l.done)
		l.mu.Unlock()
		if err := l.Listener.Close(); err != nil {
			result = fmt.Errorf("public listener close: %w", err)
		}
	})
	return result
}

func (l *boundedPublicListener) closeActive() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	connections := make([]*boundedPublicConn, 0, len(l.active))
	for connection := range l.active {
		connections = append(connections, connection)
	}
	l.mu.Unlock()
	var result error
	for _, connection := range connections {
		result = errors.Join(result, connection.Close())
	}
	return result
}

func (c *boundedPublicConn) Close() error {
	var result error
	c.once.Do(func() {
		if err := c.Conn.Close(); err != nil {
			result = fmt.Errorf("public connection close: %w", err)
		}
		c.owner.mu.Lock()
		c.owner.releaseLocked(c)
		c.owner.mu.Unlock()
	})
	return result
}
