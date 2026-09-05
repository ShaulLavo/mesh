// Package sshd exposes Mesh through a public-key-only SSH server.
package sshd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	charmssh "github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/logging"
	"github.com/charmbracelet/wish/ratelimiter"
	"github.com/charmbracelet/wish/recover"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/time/rate"
)

const (
	authorizedKeysMaximum = 1 << 20
	helloCommand          = "hello"
	helloMessage          = "mesh ssh ready\n"

	// maximumConnections caps concurrent SSH connections, the way
	// maximumPublicConnections caps the public HTTP edge. The daemon shares one
	// descriptor table across the Unix socket, SQLite, HTTPS and the edge, so an
	// unbounded SSH listener is a way to starve all of them.
	maximumConnections = 256
)

// loginGrace bounds how long an unauthenticated connection may hold a goroutine
// and a descriptor, the way OpenSSH's LoginGraceTime does. It is cleared the
// moment a key is accepted, because sessions are long-lived and a server-wide
// MaxTimeout would cut them off mid-work. A variable so tests need not wait it
// out in real time.
var loginGrace = 30 * time.Second

// gracedConnKey addresses the wrapped connection inside the SSH context.
type gracedConnKey struct{}

// gracedConn carries the pre-authentication deadline set by ConnCallback so the
// public key handler can lift it once the client proves who it is.
type gracedConn struct {
	net.Conn
	once sync.Once
}

func (c *gracedConn) authenticated() {
	c.once.Do(func() { _ = c.SetDeadline(time.Time{}) })
}

// boundedListener refuses to hold more than maximum live connections. Accepting
// and immediately closing keeps the listener backlog draining rather than
// letting the kernel queue grow behind a server that will never catch up.
type boundedListener struct {
	net.Listener
	slots chan struct{}
}

type boundedConn struct {
	net.Conn
	release func()
}

func newBoundedListener(listener net.Listener, maximum int) *boundedListener {
	return &boundedListener{Listener: listener, slots: make(chan struct{}, maximum)}
}

func (l *boundedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err //nolint:wrapcheck // the caller reports listener failures with its own context
		}
		select {
		case l.slots <- struct{}{}:
			var once sync.Once
			return &boundedConn{Conn: conn, release: func() {
				once.Do(func() { <-l.slots })
			}}, nil
		default:
			_ = conn.Close()
		}
	}
}

func (c *boundedConn) Close() error {
	defer c.release()
	return c.Conn.Close() //nolint:wrapcheck // the caller only needs to know the close failed
}

// Config identifies one concrete SSH listener and its authentication state.
// Addr must include both the discovered Tailnet address and the SSH port.
type Config struct {
	HostKey        ed25519.PrivateKey
	AuthorizedKeys string
	Addr           string
	Handler        SessionHandler
}

type normalizedConfig struct {
	hostKey        ed25519.PrivateKey
	authorizedKeys string
	addr           netip.AddrPort
	handler        SessionHandler
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

	server, err := newServer(normalized, opts...)
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
	stopShutdown := context.AfterFunc(ctx, func() {
		_ = listener.Close()
		_ = server.Close()
	})
	serveErr := server.Serve(bounded)
	stopShutdown()
	closeErr := errors.Join(normalCloseError(listener.Close()), normalCloseError(server.Close()))
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
	return normalizedConfig{hostKey: hostKey, authorizedKeys: authorizedKeys, addr: addr, handler: cfg.Handler}, nil
}

func newServer(cfg normalizedConfig, opts ...charmssh.Option) (*charmssh.Server, error) {
	hostKeyPEM, signer, err := marshalHostKey(cfg.hostKey)
	if err != nil {
		return nil, err
	}
	serverOptions := append([]charmssh.Option(nil), opts...)
	serverOptions = append(serverOptions, wish.WithHostKeyPEM(hostKeyPEM))
	server, err := wish.NewServer(serverOptions...)
	if err != nil {
		return nil, fmt.Errorf("sshd: configure Wish server: %w", err)
	}

	// Extension options are trusted to add handlers, not to weaken the public
	// boundary. Reassert every security-sensitive field after applying them.
	server.Addr = cfg.addr.String()
	server.HostSigners = []charmssh.Signer{signer}
	server.ServerConfigCallback = nil
	server.PasswordHandler = nil
	server.KeyboardInteractiveHandler = nil
	server.ConnCallback = func(ctx charmssh.Context, conn net.Conn) net.Conn {
		_ = conn.SetDeadline(time.Now().Add(loginGrace))
		graced := &gracedConn{Conn: conn}
		ctx.SetValue(gracedConnKey{}, graced)
		return graced
	}
	server.PublicKeyHandler = func(ctx charmssh.Context, key charmssh.PublicKey) bool {
		if !isAuthorized(cfg.authorizedKeys, key) {
			return false
		}
		// Only a proven client gets to hold the connection indefinitely.
		if graced, ok := ctx.Value(gracedConnKey{}).(*gracedConn); ok {
			graced.authenticated()
		}
		return true
	}
	handler := server.Handler
	if handler == nil {
		handler = helloHandler
	}
	configureSessions(server, cfg.handler, handler)
	return server, nil
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

func secureMiddleware(handler charmssh.Handler) charmssh.Handler {
	limiter := ratelimiter.NewRateLimiter(rate.Every(100*time.Millisecond), 20, 1024)
	noOp := func(charmssh.Session) {}
	return recover.Middleware(
		fixedHandler(handler),
		ratelimiter.Middleware(limiter),
		logging.Middleware(),
	)(noOp)
}

func fixedHandler(handler charmssh.Handler) wish.Middleware {
	return func(charmssh.Handler) charmssh.Handler { return handler }
}

func helloHandler(session charmssh.Session) {
	_, _ = io.WriteString(session, helloMessage)
}

func isAuthorized(path string, presented charmssh.PublicKey) bool {
	contents, err := readAuthorizedKeys(path)
	if err != nil {
		return false
	}
	for len(contents) > 0 {
		key, _, options, rest, err := gossh.ParseAuthorizedKey(contents)
		if err != nil {
			return false
		}
		if len(options) == 0 && charmssh.KeysEqual(key, presented) {
			return true
		}
		contents = rest
	}
	return false
}

func readAuthorizedKeys(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("sshd: inspect authorized_keys %s: %w", path, err)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("sshd: authorized_keys %s is not a regular file", path)
	}
	file, err := os.Open(path) //nolint:gosec // the daemon supplies its fixed state-directory authorized_keys path
	if err != nil {
		return nil, fmt.Errorf("sshd: open authorized_keys %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // a read-only authentication attempt has no close result to preserve
	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("sshd: inspect opened authorized_keys %s: %w", path, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("sshd: authorized_keys %s changed while opening", path)
	}
	permissions := opened.Mode().Perm()
	if permissions&0o022 != 0 {
		return nil, fmt.Errorf("sshd: authorized_keys %s has unsafe permissions %04o", path, permissions)
	}
	if permissions&0o444 == 0 {
		return nil, fmt.Errorf("sshd: authorized_keys %s is not readable", path)
	}
	contents, err := io.ReadAll(io.LimitReader(file, authorizedKeysMaximum+1))
	if err != nil {
		return nil, fmt.Errorf("sshd: read authorized_keys %s: %w", path, err)
	}
	if len(contents) > authorizedKeysMaximum {
		return nil, fmt.Errorf("sshd: authorized_keys %s exceeds %d bytes", path, authorizedKeysMaximum)
	}
	return contents, nil
}

func normalCloseError(err error) error {
	if errors.Is(err, net.ErrClosed) || errors.Is(err, charmssh.ErrServerClosed) {
		return nil
	}
	return err
}
