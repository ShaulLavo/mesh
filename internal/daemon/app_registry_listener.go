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
	maximumAppRegistrySourceConnections = 32
	maximumAppRegistryAuthenticating    = 32
	appRegistryProxyHeaderTimeout       = 2 * time.Second
)

type boundedAppRegistryListener struct {
	net.Listener
	maximum   int
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	closed    bool
	active    map[*boundedAppRegistryConn]struct{}
	sources   map[netip.Prefix]int
	admitted  int
	proxyUIDs []uint32
	pending   chan struct{}
}

type boundedAppRegistryConn struct {
	net.Conn
	owner     *boundedAppRegistryListener
	once      sync.Once
	identify  sync.Once
	source    netip.Prefix
	admitted  bool
	closed    bool
	idleSince time.Time
	pending   bool
	authTimer *time.Timer
}

func newBoundedAppRegistryListener(listener net.Listener, maximum int) *boundedAppRegistryListener {
	return &boundedAppRegistryListener{
		Listener: listener, maximum: maximum, done: make(chan struct{}),
		active: make(map[*boundedAppRegistryConn]struct{}), sources: make(map[netip.Prefix]int),
		pending: make(chan struct{}, maximumAppRegistryAuthenticating),
	}
}

func (l *boundedAppRegistryListener) Accept() (net.Conn, error) {
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

func (l *boundedAppRegistryListener) acceptTracked() (*boundedAppRegistryConn, error) {
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
	tracked := &boundedAppRegistryConn{Conn: connection, owner: l, pending: proxy}
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
		tracked.authTimer = time.AfterFunc(appRegistryProxyHeaderTimeout, func() { l.expireAuthentication(tracked) })
	}
	return tracked, nil
}

func (l *boundedAppRegistryListener) expireAuthentication(c *boundedAppRegistryConn) {
	l.mu.Lock()
	if !c.pending {
		l.mu.Unlock()
		return
	}
	l.releaseLocked(c)
	l.mu.Unlock()
	_ = c.Close()
}

func appRegistryConnectionSource(address net.Addr) netip.Prefix {
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

func (c *boundedAppRegistryConn) RemoteAddr() net.Addr {
	address := c.Conn.RemoteAddr()
	c.identify.Do(func() {
		if c.owner.proxyUIDs != nil {
			proxy, ok := c.Conn.(interface{ Authenticated() bool })
			if !ok || !proxy.Authenticated() {
				_ = c.Close()
				return
			}
		}
		if !c.owner.admit(c, appRegistryConnectionSource(address)) {
			_ = c.Close()
		}
	})
	return address
}

func (c *boundedAppRegistryConn) Read(p []byte) (int, error) {
	c.RemoteAddr()
	n, err := c.Conn.Read(p)
	if err != nil {
		return n, err //nolint:wrapcheck // net.Conn preserves timeout and EOF semantics
	}
	return n, nil
}

func (l *boundedAppRegistryListener) admit(c *boundedAppRegistryConn, source netip.Prefix) bool {
	l.mu.Lock()
	if l.closed || c.closed || !source.IsValid() || l.sources[source] >= maximumAppRegistrySourceConnections {
		l.mu.Unlock()
		return false
	}
	var oldest *boundedAppRegistryConn
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

func (l *boundedAppRegistryListener) finishAuthenticationLocked(c *boundedAppRegistryConn) {
	if c.authTimer != nil {
		c.authTimer.Stop()
	}
	if c.pending {
		c.pending = false
		<-l.pending
	}
}

func (l *boundedAppRegistryListener) releaseLocked(c *boundedAppRegistryConn) {
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

func (l *boundedAppRegistryListener) connState(connection net.Conn, state http.ConnState) {
	if secure, ok := connection.(*tls.Conn); ok {
		connection = secure.NetConn()
	}
	tracked, ok := connection.(*boundedAppRegistryConn)
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

func (l *boundedAppRegistryListener) Close() error {
	var result error
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		close(l.done)
		l.mu.Unlock()
		if err := l.Listener.Close(); err != nil {
			result = fmt.Errorf("app registry listener close: %w", err)
		}
	})
	return result
}

func (l *boundedAppRegistryListener) closeActive() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	connections := make([]*boundedAppRegistryConn, 0, len(l.active))
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

func (c *boundedAppRegistryConn) Close() error {
	var result error
	c.once.Do(func() {
		if err := c.Conn.Close(); err != nil {
			result = fmt.Errorf("app registry connection close: %w", err)
		}
		c.owner.mu.Lock()
		c.owner.releaseLocked(c)
		c.owner.mu.Unlock()
	})
	return result
}
