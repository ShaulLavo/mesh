package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/sshd"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/transport"
)

const (
	daemonSocketName = "daemon.sock"
	daemonLockName   = "daemon.lock"

	staleSocketProbeTimeout  = 200 * time.Millisecond
	httpReadHeaderTimeout    = 5 * time.Second
	publicReadTimeout        = 30 * time.Second
	httpShutdownTimeout      = 2 * time.Second
	maximumPublicConnections = 512
	maximumPublicHeaderBytes = 64 << 10
	connectionRefusalTimeout = 100 * time.Millisecond

	// These budgets are independent; the Tailnet budget spans every bound address.
	DefaultUnixConnectionLimit    = 128
	DefaultTailnetConnectionLimit = 128
)

// A replacement entry gets the current time, even if the filesystem reuses the
// daemon socket's inode. The deliberately old timestamp is an ownership marker.
var unixSocketOwnershipTime = time.Unix(946684800, 123456789)

// ErrDaemonAlreadyRunning reports another daemon holding the state-directory
// lock or answering on the daemon socket.
var ErrDaemonAlreadyRunning = errors.New("daemon: already running")

// ListenerConfig identifies the local daemon socket and optional Tailnet HTTP
// listeners. TailnetPort and WebSocketPath are required when TailnetAddrs is
// non-empty. HTTPSPort binds a service-only TLS listener to loopback and
// requires TLSConfig.GetCertificate. HTTPHandler receives HTTPS requests and
// Tailnet HTTP requests outside WebSocketPath, after the Host policy accepts a
// bound IP, TailnetNames entry, current PrivateName, or a public name from
// TrustPublicEdgeForwarding. ReportError receives non-fatal listener errors and
// may be nil. RequireAllTailnetListeners turns any
// discovered-address bind failure into a startup failure. Zero connection caps
// select the defaults; negative caps are rejected.
type ListenerConfig struct {
	UnixConnectionLimit        int
	TailnetConnectionLimit     int
	StateDir                   string
	TailnetAddrs               []string
	TailnetNames               []string
	PrivateName                func() string
	TrustPublicEdgeForwarding  func(netip.Addr) bool
	TailnetPort                uint16
	WebSocketPath              string
	HTTPHandler                http.Handler
	HTTPSPort                  uint16
	TLSConfig                  *tls.Config
	TailnetOwnerAccess         bool
	PublicListenAddress        string
	PublicHTTPHandler          http.Handler
	PublicTLSConfig            *tls.Config
	RequireAllTailnetListeners bool
	ReportError                func(error)
}

type listenerConfig struct {
	controlAuth                *transport.Authentication
	unixConnectionLimit        int
	tailnetConnectionLimit     int
	listen                     func(string, string) (net.Listener, error)
	stateDir                   string
	tailnetAddrs               []netip.Addr
	tailnetPort                uint16
	webSocketPath              string
	httpHandler                http.Handler
	httpHosts                  httpHostPolicy
	httpsPort                  uint16
	tlsConfig                  *tls.Config
	tailnetOwnerAccess         bool
	proxyForwarderUIDs         []uint32
	publicListenAddress        string
	publicHTTPHandler          http.Handler
	publicTLSConfig            *tls.Config
	publicReadTimeout          time.Duration
	requireAllTailnetListeners bool
	shutdownTimeout            time.Duration
	reporter                   *errorReporter
	ready                      func(context.Context) error
	sshConfigs                 []sshd.Config
	serveSSH                   func(context.Context, sshd.Config) error
}

// Serve runs the daemon's Unix and optional WebSocket listeners until ctx is
// cancelled or the required Unix listener fails. Serve closes client
// connections, but it never opens, signals, or waits for a session worker.
func Serve(ctx context.Context, cfg ListenerConfig, handler transport.Handler) error {
	normalized, err := validateListenerConfig(ctx, cfg, handler)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}

	lock, err := acquireDaemonLock(filepath.Join(normalized.stateDir, daemonLockName))
	if err != nil {
		return err
	}
	defer lock.release() //nolint:errcheck // a held lock is released by Close even if unlock reports an error

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	return serveListeners(runCtx, cancel, normalized, handler)
}

// serveListeners runs without acquiring daemon.lock. Its caller must hold that
// lock for the full call. cancel must cancel ctx and every daemon component that
// shares its lifetime, such as catalog polling and lifecycle publication.
func serveListeners(ctx context.Context, cancel context.CancelFunc, normalized listenerConfig, handler transport.Handler) error {
	defer cancel()
	defer closeListenerDiagnostics(normalized)
	if ctx.Err() != nil {
		return nil
	}

	unixListener, err := listenDaemonUnix(filepath.Join(normalized.stateDir, daemonSocketName))
	if err != nil {
		return err
	}

	tailnetListeners, bindErrors := listenTailnet(normalized.tailnetAddrs, normalized.tailnetPort, normalized.listen)
	if len(normalized.tailnetAddrs) > 0 && (len(tailnetListeners) == 0 || normalized.requireAllTailnetListeners && len(bindErrors) > 0) {
		closeErr := unixListener.Close()
		for _, listener := range tailnetListeners {
			closeErr = errors.Join(closeErr, listener.Close())
		}
		return errors.Join(fmt.Errorf("daemon: bind tailnet listeners: %w", errors.Join(bindErrors...)), closeErr)
	}
	for _, bindErr := range bindErrors {
		normalized.reporter.report(bindErr)
	}
	var httpsListener net.Listener
	if normalized.httpsPort != 0 {
		httpsListener, err = normalized.listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(normalized.httpsPort))))
		if err != nil {
			closeErrors := []error{unixListener.Close()}
			for _, listener := range tailnetListeners {
				closeErrors = append(closeErrors, listener.Close())
			}
			return errors.Join(fmt.Errorf("daemon: bind HTTPS loopback listener: %w", err), errors.Join(closeErrors...))
		}
	}
	var publicListener net.Listener
	if normalized.publicListenAddress != "" {
		publicListener, err = normalized.listen("tcp", normalized.publicListenAddress)
		if err != nil {
			closeErrors := []error{unixListener.Close()}
			for _, listener := range tailnetListeners {
				closeErrors = append(closeErrors, listener.Close())
			}
			if httpsListener != nil {
				closeErrors = append(closeErrors, httpsListener.Close())
			}
			return errors.Join(fmt.Errorf("daemon: bind public edge listener: %w", err), errors.Join(closeErrors...))
		}
	}
	return serveBoundListeners(ctx, cancel, normalized, handler, unixListener, tailnetListeners, httpsListener, publicListener)
}

// Listener teardown stops public producers before this flush; early failures
// also flush before the general reporter is retired.
func closeListenerDiagnostics(config listenerConfig) {
	if registry, ok := config.publicHTTPHandler.(interface{ Close() }); ok {
		registry.Close()
	}
	config.reporter.shutdown()
}

func serveBoundListeners(
	ctx context.Context,
	cancel context.CancelFunc,
	normalized listenerConfig,
	handler transport.Handler,
	unixListener net.Listener,
	tailnetListeners []net.Listener,
	httpsListener net.Listener,
	publicListener net.Listener,
) error {
	defer closeListenerDiagnostics(normalized)
	var boundedPublic *boundedPublicListener
	if publicListener != nil {
		boundedPublic = newBoundedPublicListener(publicListener, maximumPublicConnections)
		publicListener = boundedPublic
		if normalized.tailnetOwnerAccess {
			boundedPublic.proxyUIDs = append([]uint32{}, normalized.proxyForwarderUIDs...)
		}
	}
	// Failed discovery binds must not establish non-loopback IP authorities.
	normalized.httpHosts.tailnetAddrs = boundHTTPAddresses(tailnetListeners)
	connections := newConnectionGroup(handler)
	connections.name, connections.limit = "Unix", normalized.unixConnectionLimit
	tailnetConnections := newConnectionGroup(handler)
	tailnetConnections.name, tailnetConnections.limit = "Tailnet", normalized.tailnetConnectionLimit
	server := newWebSocketServer(ctx, normalized, tailnetConnections)
	var httpsServer *http.Server
	if httpsListener != nil {
		httpsServer = &http.Server{
			Handler:           serviceOnlyHTTPSHandler(normalized),
			ReadHeaderTimeout: httpReadHeaderTimeout,
			BaseContext:       func(net.Listener) context.Context { return ctx },
			TLSConfig:         normalized.tlsConfig,
		}
	}
	var publicServer *http.Server
	if publicListener != nil {
		readTimeout := normalized.publicReadTimeout
		if readTimeout == 0 {
			readTimeout = publicReadTimeout
		}
		publicServer = &http.Server{
			Handler: guardPublicBody(normalized.publicHTTPHandler, readTimeout), ReadHeaderTimeout: httpReadHeaderTimeout,
			ConnState: boundedPublic.connState, ConnContext: publicConnectionContext, IdleTimeout: readTimeout, MaxHeaderBytes: maximumPublicHeaderBytes,
			BaseContext: func(net.Listener) context.Context { return ctx }, TLSConfig: normalized.publicTLSConfig,
			ErrorLog: log.New(io.Discard, "", 0),
		}
	}
	var listenerWG sync.WaitGroup
	fatal := make(chan error, 1)

	listenerWG.Go(func() {
		if acceptErr := serveUnixConnections(ctx, unixListener, connections); acceptErr != nil {
			select {
			case fatal <- acceptErr:
			default:
			}
		}
	})
	for _, listener := range tailnetListeners {
		listener := listener
		listenerWG.Go(func() {
			serveErr := server.Serve(listener)
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && ctx.Err() == nil {
				normalized.reporter.report(fmt.Errorf("daemon: serve WebSocket on %s: %w", listener.Addr(), serveErr))
			}
		})
	}
	if httpsServer != nil {
		listenerWG.Go(func() {
			serveErr := httpsServer.ServeTLS(httpsListener, "", "")
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && ctx.Err() == nil {
				select {
				case fatal <- fmt.Errorf("daemon: serve HTTPS on %s: %w", httpsListener.Addr(), serveErr):
				default:
				}
			}
		})
	}
	if publicServer != nil {
		listenerWG.Go(func() {
			var serveErr error
			if normalized.publicTLSConfig != nil {
				serveErr = publicServer.ServeTLS(publicListener, "", "")
			} else {
				serveErr = publicServer.Serve(publicListener)
			}
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && ctx.Err() == nil {
				select {
				case fatal <- fmt.Errorf("daemon: serve public edge on %s: %w", publicListener.Addr(), serveErr):
				default:
				}
			}
		})
	}
	for _, sshConfig := range normalized.sshConfigs {
		sshConfig := sshConfig
		listenerWG.Go(func() {
			serveErr := normalized.serveSSH(ctx, sshConfig)
			if ctx.Err() != nil {
				return
			}
			if serveErr == nil {
				serveErr = errors.New("SSH server stopped unexpectedly")
			}
			select {
			case fatal <- fmt.Errorf("daemon: serve SSH on %s: %w", sshConfig.Addr, serveErr):
			default:
			}
		})
	}
	var runErr error
	if normalized.ready != nil {
		if err := normalized.ready(ctx); err != nil && ctx.Err() == nil {
			runErr = err
		}
	}
	if runErr == nil {
		select {
		case <-ctx.Done():
		case runErr = <-fatal:
		}
	}
	// Fatal listener failure is also daemon shutdown. Cancel the shared lifetime
	// before closing sockets or waiting, so handlers blocked in publication can
	// observe it and return.
	cancel()

	closeErr := unixListener.Close()
	closeErr = errors.Join(closeErr, connections.closeAll(), tailnetConnections.closeAll())
	if err := shutdownHTTPServer(server.Server, normalized.shutdownTimeout); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("daemon: close WebSocket server: %w", err))
	}
	if httpsServer != nil {
		if err := shutdownHTTPServer(httpsServer, normalized.shutdownTimeout); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("daemon: close HTTPS server: %w", err))
		}
	}
	if publicServer != nil {
		if err := shutdownHTTPServer(publicServer, normalized.shutdownTimeout); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("daemon: close public edge server: %w", err))
		}
		closeErr = errors.Join(closeErr, boundedPublic.closeActive())
	}
	listenerWG.Wait()
	connections.wait()
	tailnetConnections.wait()
	return errors.Join(runErr, closeErr)
}

func validateTailnetOwnerAccess(cfg ListenerConfig) ([]uint32, error) {
	if !cfg.TailnetOwnerAccess {
		return nil, nil
	}
	address, err := netip.ParseAddrPort(cfg.PublicListenAddress)
	if err != nil || !address.Addr().IsLoopback() || cfg.PublicTLSConfig == nil {
		return nil, errors.New("daemon: Tailnet owner access requires a loopback public TLS listener")
	}
	uids, err := tailnet.ProxyForwarderUIDs()
	if err != nil {
		return nil, fmt.Errorf("daemon: enable Tailnet owner access: %w", err)
	}
	return uids, nil
}

func validateListenerConfig(ctx context.Context, cfg ListenerConfig, handler transport.Handler) (listenerConfig, error) {
	if ctx == nil {
		return listenerConfig{}, errors.New("daemon: nil context")
	}
	if handler == nil {
		return listenerConfig{}, errors.New("daemon: nil connection handler")
	}
	if cfg.StateDir == "" {
		return listenerConfig{}, errors.New("daemon: state directory is empty")
	}
	info, err := os.Stat(cfg.StateDir)
	if err != nil {
		return listenerConfig{}, fmt.Errorf("daemon: inspect state directory %s: %w", cfg.StateDir, err)
	}
	if !info.IsDir() {
		return listenerConfig{}, fmt.Errorf("daemon: state directory %s is not a directory", cfg.StateDir)
	}

	controlAuth, err := controlAuthentication(cfg.StateDir)
	if err != nil {
		return listenerConfig{}, err
	}

	if cfg.UnixConnectionLimit < 0 || cfg.TailnetConnectionLimit < 0 {
		return listenerConfig{}, errors.New("daemon: control connection caps must be positive; zero selects the default")
	}
	if cfg.UnixConnectionLimit == 0 {
		cfg.UnixConnectionLimit = DefaultUnixConnectionLimit
	}
	if cfg.TailnetConnectionLimit == 0 {
		cfg.TailnetConnectionLimit = DefaultTailnetConnectionLimit
	}
	normalized := listenerConfig{
		controlAuth:                controlAuth,
		unixConnectionLimit:        cfg.UnixConnectionLimit,
		tailnetConnectionLimit:     cfg.TailnetConnectionLimit,
		listen:                     net.Listen,
		stateDir:                   filepath.Clean(cfg.StateDir),
		tailnetPort:                cfg.TailnetPort,
		webSocketPath:              cfg.WebSocketPath,
		httpHandler:                cfg.HTTPHandler,
		httpsPort:                  cfg.HTTPSPort,
		publicListenAddress:        cfg.PublicListenAddress,
		tailnetOwnerAccess:         cfg.TailnetOwnerAccess,
		publicHTTPHandler:          cfg.PublicHTTPHandler,
		publicReadTimeout:          publicReadTimeout,
		requireAllTailnetListeners: cfg.RequireAllTailnetListeners,
		shutdownTimeout:            httpShutdownTimeout,
		reporter:                   newErrorReporter(cfg.ReportError),
		httpHosts: httpHostPolicy{
			tailnetNames:              append([]string(nil), cfg.TailnetNames...),
			privateName:               cfg.PrivateName,
			trustPublicEdgeForwarding: cfg.TrustPublicEdgeForwarding,
		},
	}
	normalized.proxyForwarderUIDs, err = validateTailnetOwnerAccess(cfg)
	if err != nil {
		return listenerConfig{}, err
	}
	if cfg.HTTPSPort == 0 && cfg.TLSConfig != nil {
		return listenerConfig{}, errors.New("daemon: TLS config requires a non-zero HTTPS port")
	}
	if cfg.PublicListenAddress == "" {
		if cfg.PublicHTTPHandler != nil || cfg.PublicTLSConfig != nil {
			return listenerConfig{}, errors.New("daemon: public edge handler or TLS config requires a listen address")
		}
	} else {
		if cfg.PublicHTTPHandler == nil {
			return listenerConfig{}, errors.New("daemon: public edge listener requires an HTTP handler")
		}
		if cfg.PublicTLSConfig != nil {
			if cfg.PublicTLSConfig.GetCertificate == nil {
				return listenerConfig{}, errors.New("daemon: public TLS listener requires GetCertificate")
			}
			normalized.publicTLSConfig = cfg.PublicTLSConfig.Clone()
			if normalized.publicTLSConfig.MinVersion == 0 {
				normalized.publicTLSConfig.MinVersion = tls.VersionTLS12
			}
			if normalized.publicTLSConfig.MinVersion < tls.VersionTLS12 {
				return listenerConfig{}, errors.New("daemon: public TLS listener requires TLS 1.2 or newer")
			}
		}
	}
	if cfg.HTTPSPort != 0 {
		if cfg.HTTPHandler == nil {
			return listenerConfig{}, errors.New("daemon: HTTPS listener requires an HTTP service handler")
		}
		if cfg.TLSConfig == nil || cfg.TLSConfig.GetCertificate == nil {
			return listenerConfig{}, errors.New("daemon: HTTPS listener requires TLS GetCertificate")
		}
		normalized.tlsConfig = cfg.TLSConfig.Clone()
		if normalized.tlsConfig.MinVersion == 0 {
			normalized.tlsConfig.MinVersion = tls.VersionTLS12
		}
		if normalized.tlsConfig.MinVersion < tls.VersionTLS12 {
			return listenerConfig{}, errors.New("daemon: HTTPS listener requires TLS 1.2 or newer")
		}
		if err := validateWebSocketPath(cfg.WebSocketPath); err != nil {
			return listenerConfig{}, err
		}
	}
	if len(cfg.TailnetAddrs) == 0 {
		return normalized, nil
	}
	if cfg.TailnetPort == 0 {
		return listenerConfig{}, errors.New("daemon: Tailnet port must be non-zero when addresses are supplied")
	}
	if err := validateWebSocketPath(cfg.WebSocketPath); err != nil {
		return listenerConfig{}, err
	}

	seen := make(map[netip.Addr]struct{}, len(cfg.TailnetAddrs))
	for _, text := range cfg.TailnetAddrs {
		addr, err := netip.ParseAddr(text)
		if err != nil {
			return listenerConfig{}, fmt.Errorf("daemon: parse Tailnet address %q: %w", text, err)
		}
		addr = addr.Unmap()
		if addr.IsUnspecified() || addr.IsMulticast() {
			return listenerConfig{}, fmt.Errorf("daemon: Tailnet address %q is not a concrete unicast address", text)
		}
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		normalized.tailnetAddrs = append(normalized.tailnetAddrs, addr)
	}
	return normalized, nil
}

func serviceOnlyHTTPSHandler(cfg listenerConfig) http.Handler {
	services := privateHTTPHandler(cfg.httpHandler, cfg.httpHosts)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() == cfg.webSocketPath || r.URL.Path == cfg.webSocketPath {
			http.NotFound(w, r)
			return
		}
		services.ServeHTTP(w, r)
	})
}

func validateWebSocketPath(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || value == "" || value[0] != '/' || strings.Contains(value, "\\") || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Opaque != "" || parsed.ForceQuery || parsed.EscapedPath() != value {
		return fmt.Errorf("daemon: WebSocket path %q must be an absolute path without a query or fragment", value)
	}
	if cleaned := path.Clean(value); cleaned != value {
		return fmt.Errorf("daemon: WebSocket path %q is not clean", value)
	}
	return nil
}

func listenTailnet(addrs []netip.Addr, port uint16, listen func(string, string) (net.Listener, error)) ([]net.Listener, []error) {
	listeners := make([]net.Listener, 0, len(addrs))
	var listenErrors []error
	service := strconv.Itoa(int(port))
	for _, addr := range addrs {
		network := "tcp6"
		if addr.Is4() {
			network = "tcp4"
		}
		endpoint := net.JoinHostPort(addr.String(), service)
		listener, err := listen(network, endpoint)
		if err != nil {
			listenErrors = append(listenErrors, fmt.Errorf("daemon: bind Tailnet address %s: %w", endpoint, err))
			continue
		}
		listeners = append(listeners, listener)
	}
	return listeners, listenErrors
}

type webSocketServer struct {
	*http.Server
}

func newWebSocketServer(ctx context.Context, cfg listenerConfig, connections *connectionGroup) *webSocketServer {
	server := &webSocketServer{}
	services := privateHTTPHandler(cfg.httpHandler, cfg.httpHosts)
	serveHTTP := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != cfg.webSocketPath {
			services.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(w, "browser control connections are forbidden", http.StatusForbidden)
			return
		}
		id, err := connections.reserve()
		if err != nil {
			if !errors.Is(err, transport.ErrClosed) {
				w.Header().Set(transport.ControlConnectionLimitHeader, strconv.Itoa(connections.limit))
			}
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer connections.release(id)
		if cfg.controlAuth == nil {
			http.Error(w, "Mesh device authorization unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: cfg.controlAuth}, func(connectionCtx context.Context, conn transport.Conn) error {
			if _, authenticated := transport.Peer(connectionCtx); !authenticated {
				return transport.ErrAuthentication
			}
			handlerCtx, cancel := context.WithCancel(connectionCtx)
			stop := context.AfterFunc(ctx, cancel)
			defer func() {
				stop()
				cancel()
			}()
			return connections.run(id, handlerCtx, conn)
		})
	})
	server.Server = &http.Server{
		Handler:           serveHTTP,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	return server
}

func shutdownHTTPServer(server *http.Server, timeout time.Duration) error {
	if server == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		closeErr := server.Close()
		if errors.Is(closeErr, http.ErrServerClosed) {
			closeErr = nil
		}
		return errors.Join(err, closeErr)
	}
	return nil
}

func serveUnixConnections(ctx context.Context, listener net.Listener, connections *connectionGroup) error {
	for {
		stream, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("daemon: accept Unix connection: %w", err)
		}
		conn, err := transport.NewStreamConn(stream)
		if err != nil {
			_ = stream.Close()
			continue
		}
		id, err := connections.reserve()
		if err != nil {
			refuseUnixConnection(stream, conn, err)
			continue
		}
		go func() {
			defer connections.release(id)
			_ = connections.run(id, context.WithValue(ctx, localClientKey{}, true), conn)
		}()
	}
}

type connectionGroup struct {
	handler transport.Handler
	name    string
	limit   int
	mu      sync.Mutex
	nextID  uint64
	closed  bool
	conns   map[uint64]transport.Conn
	wg      sync.WaitGroup
}

func newConnectionGroup(handler transport.Handler) *connectionGroup {
	return &connectionGroup{handler: handler, conns: make(map[uint64]transport.Conn)}
}

// Refusals read one request so older clients receive their matching error.
// A deadline bounds this work without allocating another handler goroutine.
func refuseUnixConnection(stream net.Conn, conn transport.Conn, refusal error) {
	defer conn.Close() //nolint:errcheck // refused socket is disposable
	_ = stream.SetDeadline(time.Now().Add(connectionRefusalTimeout))
	frame, err := conn.ReadFrame()
	if err != nil || frame.Kind != protocol.KindControl {
		return
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return
	}
	payload, err := (protocol.Control{Type: protocol.TypeError, RequestID: request.RequestID, Message: refusal.Error()}).Encode()
	if err == nil {
		_ = conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
	}
}

func (g *connectionGroup) reserve() (uint64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return 0, transport.ErrClosed
	}
	if g.limit > 0 && len(g.conns) >= g.limit {
		return 0, fmt.Errorf("daemon: %s control connection cap (%d) reached", g.name, g.limit)
	}
	g.nextID++
	id := g.nextID
	// Reservations also count before WebSocket upgrade allocates its queues.
	g.conns[id] = nil
	g.wg.Add(1)
	return id, nil
}

func (g *connectionGroup) release(id uint64) {
	g.mu.Lock()
	delete(g.conns, id)
	g.mu.Unlock()
	g.wg.Done()
}

func (g *connectionGroup) run(id uint64, ctx context.Context, conn transport.Conn) error {
	defer conn.Close() //nolint:errcheck // connection teardown
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return transport.ErrClosed
	}
	g.conns[id] = conn
	g.mu.Unlock()
	return g.handler(ctx, conn)
}

func (g *connectionGroup) closeAll() error {
	g.mu.Lock()
	g.closed = true
	connections := make([]transport.Conn, 0, len(g.conns))
	for _, conn := range g.conns {
		if conn != nil {
			connections = append(connections, conn)
		}
	}
	g.mu.Unlock()

	errs := make(chan error, len(connections))
	var wg sync.WaitGroup
	for _, conn := range connections {
		conn := conn
		wg.Go(func() {
			if err := conn.Close(); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	var closeErrors []error
	for err := range errs {
		closeErrors = append(closeErrors, err)
	}
	return errors.Join(closeErrors...)
}

func (g *connectionGroup) wait() {
	g.wg.Wait()
}

type daemonLock struct {
	file *os.File
}

func acquireDaemonLock(path string) (*daemonLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path is the fixed daemon lock filename under the validated local state directory
	if err != nil {
		return nil, fmt.Errorf("daemon: open lock %s: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("daemon: secure lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrDaemonAlreadyRunning
		}
		return nil, fmt.Errorf("daemon: lock %s: %w", path, err)
	}
	return &daemonLock{file: file}, nil
}

func (l *daemonLock) release() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	return errors.Join(unlockErr, closeErr)
}

type ownedUnixListener struct {
	*net.UnixListener
	path      string
	boundInfo os.FileInfo
	closeOnce sync.Once
	closeErr  error
}

func listenDaemonUnix(socketPath string) (*ownedUnixListener, error) {
	if err := paths.ValidateSocketPath(socketPath); err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	if err := removeStaleUnixSocket(socketPath); err != nil {
		return nil, err
	}
	temporary, err := reserveTemporarySocketPath(socketPath)
	if err != nil {
		return nil, err
	}
	if err := paths.ValidateSocketPath(temporary); err != nil {
		return nil, fmt.Errorf("daemon: temporary socket: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: temporary, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("daemon: listen on temporary Unix socket %s: %w", temporary, err)
	}
	listener.SetUnlinkOnClose(false)
	cleanupTemporary := func() {
		_ = listener.Close()
		_ = os.Remove(temporary)
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		cleanupTemporary()
		return nil, fmt.Errorf("daemon: secure Unix socket %s: %w", socketPath, err)
	}
	// A replacement filesystem entry can reuse the unlinked socket's inode.
	// Stamp this entry so cleanup can distinguish that replacement even then.
	if err := os.Chtimes(temporary, unixSocketOwnershipTime, unixSocketOwnershipTime); err != nil {
		cleanupTemporary()
		return nil, fmt.Errorf("daemon: mark Unix socket %s: %w", socketPath, err)
	}
	info, err := os.Lstat(temporary)
	if err != nil {
		cleanupTemporary()
		return nil, fmt.Errorf("daemon: inspect Unix socket %s: %w", socketPath, err)
	}
	// Publish only after permissions and ownership metadata are complete. The
	// platform no-replace rename preserves an entry created after the stale
	// socket check and avoids exposing a half-initialized listener pathname.
	if err := publishUnixSocket(temporary, socketPath); err != nil {
		cleanupTemporary()
		return nil, fmt.Errorf("daemon: publish Unix socket %s: %w", socketPath, err)
	}
	return &ownedUnixListener{UnixListener: listener, path: socketPath, boundInfo: info}, nil
}

func reserveTemporarySocketPath(socketPath string) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(socketPath), ".d-")
	if err != nil {
		return "", fmt.Errorf("daemon: reserve temporary Unix socket path: %w", err)
	}
	path := file.Name()
	closeErr := file.Close()
	removeErr := os.Remove(path)
	if err := errors.Join(closeErr, removeErr); err != nil {
		return "", fmt.Errorf("daemon: prepare temporary Unix socket path %s: %w", path, err)
	}
	return path, nil
}

func removeStaleUnixSocket(socketPath string) error {
	before, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("daemon: inspect Unix socket %s: %w", socketPath, err)
	}
	if before.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("daemon: path %s exists and is not a Unix socket", socketPath)
	}

	conn, dialErr := net.DialTimeout("unix", socketPath, staleSocketProbeTimeout)
	if dialErr == nil {
		_ = conn.Close()
		return ErrDaemonAlreadyRunning
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, syscall.ENOENT) {
		return fmt.Errorf("daemon: cannot prove Unix socket %s is stale: %w", socketPath, dialErr)
	}
	after, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("daemon: inspect stale Unix socket %s: %w", socketPath, err)
	}
	if !os.SameFile(before, after) {
		return fmt.Errorf("daemon: Unix socket %s changed while checking whether it was stale", socketPath)
	}
	if err := os.Remove(socketPath); err != nil {
		return fmt.Errorf("daemon: remove stale Unix socket %s: %w", socketPath, err)
	}
	return nil
}

func (l *ownedUnixListener) Close() error {
	l.closeOnce.Do(func() {
		current, statErr := os.Lstat(l.path)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
			statErr = nil
		case statErr == nil && current.Mode()&os.ModeSocket != 0 &&
			os.SameFile(l.boundInfo, current) && current.ModTime().Equal(l.boundInfo.ModTime()):
			statErr = os.Remove(l.path)
		case statErr == nil:
			statErr = nil
		}
		// Verify the inode, type, and ownership marker before unlinking. Closing
		// afterward cannot remove a later replacement because automatic unlinking
		// is disabled on this listener.
		listenErr := l.UnixListener.Close()
		if errors.Is(listenErr, net.ErrClosed) {
			listenErr = nil
		}
		l.closeErr = errors.Join(listenErr, statErr)
	})
	return l.closeErr
}

type errorReporter struct {
	fn    func(error)
	queue chan error
	done  chan struct{}
	stop  chan struct{}
	start sync.Once
	close sync.Once
}

func newErrorReporter(report func(error)) *errorReporter {
	if report == nil {
		report = func(err error) { log.Printf("%v", err) }
	}
	return &errorReporter{fn: report, queue: make(chan error, 64), done: make(chan struct{}), stop: make(chan struct{})}
}

func (r *errorReporter) report(err error) {
	if r == nil || err == nil {
		return
	}
	select {
	case <-r.done:
		return
	default:
	}
	r.start.Do(func() {
		go func() {
			defer close(r.stop)
			for {
				select {
				case <-r.done:
					return
				case queued := <-r.queue:
					r.fn(queued)
				}
			}
		}()
	})
	select {
	case r.queue <- err:
	case <-r.done:
	default:
	}
}

func (r *errorReporter) shutdown() {
	if r == nil {
		return
	}
	r.close.Do(func() { close(r.done) })
}
