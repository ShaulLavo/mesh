// Package sshd exposes Mesh through a public-key-only SSH server.
package sshd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	charmssh "charm.land/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/sshfs"
)

const (
	authorizedKeysMaximum = identity.AuthorizedKeysMaximum
	helloCommand          = "hello"
	helloMessage          = "mesh ssh ready\n"

	// SSH, private HTTPS and workers share one descriptor table.
	// An unbounded SSH listener could starve all of them.
	maximumConnections = 256
)

// loginGrace bounds how long an unauthenticated connection may hold a goroutine
// and a descriptor, the way OpenSSH's LoginGraceTime does. It is cleared the
// moment a signed key is verified, because sessions are long-lived and a server-wide
// MaxTimeout would cut them off mid-work.
const loginGrace = 30 * time.Second

// gracedConnKey addresses the wrapped connection inside the SSH context.
type gracedConnKey struct{}

// gracedConn carries the pre-authentication deadline set by ConnCallback so the
// verified-key callback can lift it once the client proves who it is.
type gracedConn struct {
	net.Conn
	once    sync.Once
	grantMu sync.RWMutex
	grant   func() bool
}

func (c *gracedConn) authenticated() {
	c.once.Do(func() { _ = c.SetDeadline(time.Time{}) })
}

func (c *gracedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.grantMu.RLock()
	allowed := c.grant
	c.grantMu.RUnlock()
	if allowed != nil && !allowed() {
		_ = c.Close()
		return 0, errors.New("sshd: device grant revoked")
	}
	return n, err //nolint:wrapcheck // net.Conn preserves EOF and socket errors for the SSH transport
}

func (c *gracedConn) watchGrant(ctx context.Context, allowed func() bool) {
	c.grantMu.Lock()
	if c.grant != nil {
		c.grantMu.Unlock()
		return
	}
	c.grant = allowed
	c.grantMu.Unlock()
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !allowed() {
					_ = c.Close()
					return
				}
			}
		}
	}()
}

// boundedListener owns sockets from Accept through Close, including handshakes
// that the SSH server has not registered yet. Excess connections are closed
// immediately so they cannot accumulate in the listener backlog.
type boundedListener struct {
	net.Listener
	maximum  int
	mu       sync.Mutex
	conns    map[*boundedConn]struct{}
	closed   bool
	once     sync.Once
	closeErr error
}

type boundedConn struct {
	net.Conn
	listener *boundedListener
	once     sync.Once
	closeErr error
}

func newBoundedListener(listener net.Listener, maximum int) *boundedListener {
	return &boundedListener{Listener: listener, maximum: maximum, conns: make(map[*boundedConn]struct{})}
}

func (l *boundedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err //nolint:wrapcheck // the caller reports listener failures with its own context
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			_ = conn.Close()
			return nil, net.ErrClosed
		}
		if len(l.conns) >= l.maximum {
			l.mu.Unlock()
			_ = conn.Close()
			continue
		}
		owned := &boundedConn{Conn: conn, listener: l}
		l.conns[owned] = struct{}{}
		l.mu.Unlock()
		return owned, nil
	}
}

func (l *boundedListener) Close() error {
	l.once.Do(func() {
		l.mu.Lock()
		l.closed = true
		connections := make([]*boundedConn, 0, len(l.conns))
		for conn := range l.conns {
			connections = append(connections, conn)
		}
		l.mu.Unlock()
		l.closeErr = l.Listener.Close()
		for _, conn := range connections {
			_ = conn.Close()
		}
	})
	return l.closeErr //nolint:wrapcheck // preserve the underlying listener close result
}

func (c *boundedConn) Close() error {
	c.once.Do(func() {
		c.closeErr = c.Conn.Close()
		c.listener.mu.Lock()
		delete(c.listener.conns, c)
		c.listener.mu.Unlock()
	})
	return c.closeErr //nolint:wrapcheck // preserve the underlying socket close result
}

// Config identifies one concrete SSH listener and its authentication state.
// Addr must include both the discovered Tailnet address and the SSH port.
type Config struct {
	HostKey         ed25519.PrivateKey
	AuthorizedKeys  string
	ScopeMembership func(string) (bool, error)
	Addr            string
	Handler         SessionHandler
	Services        *serve.Registry
}

type normalizedConfig struct {
	loginGrace      time.Duration
	hostKey         ed25519.PrivateKey
	authorizedKeys  string
	scopeMembership func(string) (bool, error)
	addr            netip.AddrPort
	handler         SessionHandler
	services        *serve.Registry
}

// Serve runs one locked SSH listener until ctx is done. Options may register
// handlers and protocol extensions. Serve always owns the address, host key,
// and authentication callbacks regardless of the supplied options.
func Serve(ctx context.Context, cfg Config, opts ...charmssh.Option) error {
	normalized, err := validateConfig(ctx, cfg)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}

	server, err := newServer(normalized, opts...) //nolint:contextcheck // SSH publishes its own per-connection context to the authentication callback.
	if err != nil {
		return err
	}
	network := "tcp6"
	if normalized.addr.Addr().Is4() {
		network = "tcp4"
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, network, normalized.addr.String())
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("sshd: listen on %s: %w", normalized.addr, err)
	}

	bounded := newBoundedListener(listener, maximumConnections)
	shutdownDone := make(chan struct{})
	stopShutdown := context.AfterFunc(ctx, func() {
		defer close(shutdownDone)
		_ = bounded.Close()
		_ = server.Close()
	})
	serveErr := server.Serve(bounded)
	if !stopShutdown() {
		<-shutdownDone
	}
	closeErr := errors.Join(normalCloseError(bounded.Close()), normalCloseError(server.Close()))
	if ctx.Err() != nil {
		return closeErr
	}
	if errors.Is(serveErr, charmssh.ErrServerClosed) || errors.Is(serveErr, net.ErrClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, closeErr)
}

func validateConfig(ctx context.Context, cfg Config) (normalizedConfig, error) {
	if ctx == nil {
		return normalizedConfig{}, errors.New("sshd: nil context")
	}
	if len(cfg.HostKey) != ed25519.PrivateKeySize {
		return normalizedConfig{}, fmt.Errorf("sshd: host key has %d bytes, want %d", len(cfg.HostKey), ed25519.PrivateKeySize)
	}
	hostKey := ed25519.PrivateKey(bytes.Clone(cfg.HostKey))
	if derived := ed25519.NewKeyFromSeed(hostKey.Seed()); !bytes.Equal(derived, hostKey) {
		return normalizedConfig{}, errors.New("sshd: host key is not a valid Ed25519 private key")
	}
	if cfg.ScopeMembership == nil {
		return normalizedConfig{}, errors.New("sshd: scope membership reader required")
	}
	if cfg.AuthorizedKeys == "" {
		return normalizedConfig{}, errors.New("sshd: authorized_keys path is empty")
	}
	authorizedKeys, err := filepath.Abs(cfg.AuthorizedKeys)
	if err != nil {
		return normalizedConfig{}, fmt.Errorf("sshd: resolve authorized_keys path %s: %w", cfg.AuthorizedKeys, err)
	}
	addr, err := netip.ParseAddrPort(cfg.Addr)
	if err != nil {
		return normalizedConfig{}, fmt.Errorf("sshd: parse listen address %q: %w", cfg.Addr, err)
	}
	addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	if addr.Port() == 0 || addr.Addr().IsUnspecified() || addr.Addr().IsMulticast() {
		return normalizedConfig{}, fmt.Errorf("sshd: listen address %q is not a concrete IP endpoint", cfg.Addr)
	}
	return normalizedConfig{hostKey: hostKey, authorizedKeys: authorizedKeys, scopeMembership: cfg.ScopeMembership, addr: addr, handler: cfg.Handler, services: cfg.Services}, nil
}

func newServer(cfg normalizedConfig, opts ...charmssh.Option) (*charmssh.Server, error) {
	if cfg.scopeMembership == nil {
		return nil, errors.New("sshd: scope membership reader required")
	}
	_, signer, err := marshalHostKey(cfg.hostKey)
	if err != nil {
		return nil, err
	}
	server := &charmssh.Server{}
	for _, option := range opts {
		if err := server.SetOption(option); err != nil {
			return nil, fmt.Errorf("sshd: configure SSH server: %w", err)
		}
	}

	// Extension options are trusted to add handlers, not to weaken the authentication
	// boundary. Reassert every security-sensitive field after applying them.
	server.Addr = cfg.addr.String()
	server.HostSigners = []charmssh.Signer{signer}
	server.ServerConfigCallback = nil
	server.PasswordHandler = nil
	server.KeyboardInteractiveHandler = nil
	grace := cfg.loginGrace
	if grace == 0 {
		grace = loginGrace
	}
	// The SSH transport replaces deadlines supplied by ConnCallback.
	server.HandshakeTimeout = grace
	server.ConnCallback = func(ctx charmssh.Context, conn net.Conn) net.Conn {
		_ = conn.SetDeadline(time.Now().Add(grace))
		graced := &gracedConn{Conn: conn}
		ctx.SetValue(gracedConnKey{}, graced)
		return graced
	}
	server.PublicKeyHandler = func(_ charmssh.Context, key charmssh.PublicKey) bool {
		return cfg.unscoped(key) && identity.Granted(cfg.authorizedKeys, key)
	}
	server.ServerConfigCallback = cfg.signedConfig
	handler := server.Handler
	if handler == nil {
		handler = helloHandler
	}
	var files *sshfs.Filesystem
	if cfg.services != nil {
		files = sshfs.New(cfg.services)
	}
	configureSessions(server, cfg.handler, handler, files)
	return server, nil
}

func (cfg normalizedConfig) signedConfig(ctx charmssh.Context) *gossh.ServerConfig {
	return &gossh.ServerConfig{VerifiedPublicKeyCallback: func(_ gossh.ConnMetadata, key gossh.PublicKey, permissions *gossh.Permissions, _ string) (*gossh.Permissions, error) {
		native, ok := identity.BindGrant(cfg.authorizedKeys, key)
		if !ok || !cfg.unscoped(key) {
			return nil, errors.New("sshd: signed device key is not approved")
		}
		var invalid atomic.Bool
		current := func() bool {
			if invalid.Load() {
				return false
			}
			if !native() || !cfg.unscoped(key) {
				invalid.Store(true)
				return false
			}
			return !invalid.Load()
		}
		// Unsigned public-key queries confer neither a lifetime nor an unlimited login.
		if graced, ok := ctx.Value(gracedConnKey{}).(*gracedConn); ok {
			graced.watchGrant(ctx, current)
			graced.authenticated()
		}
		return permissions, nil
	}}
}

func (cfg normalizedConfig) unscoped(key gossh.PublicKey) bool {
	subject := ""
	if cryptoKey, ok := key.(gossh.CryptoPublicKey); ok {
		if public, ok := cryptoKey.CryptoPublicKey().(ed25519.PublicKey); ok {
			subject = base64.RawURLEncoding.EncodeToString(public)
		}
	}
	// Only Ed25519 identities can be scoped. Other native SSH grant keys still
	// require a healthy scope snapshot, followed by their original native grant.

	registered, err := cfg.scopeMembership(subject)
	return err == nil && !registered
}

func marshalHostKey(private ed25519.PrivateKey) ([]byte, gossh.Signer, error) {
	block, err := gossh.MarshalPrivateKey(private, "Mesh host identity")
	if err != nil {
		return nil, nil, fmt.Errorf("sshd: marshal host key in OpenSSH format: %w", err)
	}
	encoded := pem.EncodeToMemory(block)
	signer, err := gossh.ParsePrivateKey(encoded)
	if err != nil {
		return nil, nil, fmt.Errorf("sshd: parse exported host key: %w", err)
	}
	return encoded, signer, nil
}

func helloHandler(session charmssh.Session) {
	_, _ = io.WriteString(session, helloMessage)
}

func normalCloseError(err error) error {
	if errors.Is(err, net.ErrClosed) || errors.Is(err, charmssh.ErrServerClosed) {
		return nil
	}
	return err
}
