package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/transport"
)

const (
	daemonSocketName = "daemon.sock"
	daemonLockName   = "daemon.lock"

	staleSocketProbeTimeout = 200 * time.Millisecond
	httpReadHeaderTimeout   = 5 * time.Second
)

// A replacement entry gets the current time, even if the filesystem reuses the
// daemon socket's inode. The deliberately old timestamp is an ownership marker.
var unixSocketOwnershipTime = time.Unix(946684800, 123456789)

// ErrDaemonAlreadyRunning reports another daemon holding the state-directory
// lock or answering on the daemon socket.
var ErrDaemonAlreadyRunning = errors.New("daemon: already running")

// ListenerConfig identifies the local daemon socket and optional Tailnet HTTP
// listeners. TailnetPort and WebSocketPath are required when TailnetAddrs is
// non-empty. HTTPHandler receives requests outside WebSocketPath. ReportError
// receives non-fatal listener errors and may be nil.
type ListenerConfig struct {
	StateDir      string
	TailnetAddrs  []string
	TailnetPort   uint16
	WebSocketPath string
	HTTPHandler   http.Handler
	ReportError   func(error)
}

type listenerConfig struct {
	stateDir      string
	tailnetAddrs  []netip.Addr
	tailnetPort   uint16
	webSocketPath string
	httpHandler   http.Handler
	reporter      *errorReporter
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
	if ctx.Err() != nil {
		return nil
	}

	unixListener, err := listenDaemonUnix(filepath.Join(normalized.stateDir, daemonSocketName))
	if err != nil {
		return err
	}

	tailnetListeners, bindErrors := listenTailnet(normalized.tailnetAddrs, normalized.tailnetPort)
	if len(normalized.tailnetAddrs) > 0 && len(tailnetListeners) == 0 {
		closeErr := unixListener.Close()
		return errors.Join(fmt.Errorf("daemon: bind tailnet listeners: %w", errors.Join(bindErrors...)), closeErr)
	}
	for _, bindErr := range bindErrors {
		normalized.reporter.report(bindErr)
	}
	return serveBoundListeners(ctx, cancel, normalized, handler, unixListener, tailnetListeners)
}

func serveBoundListeners(
	ctx context.Context,
	cancel context.CancelFunc,
	normalized listenerConfig,
	handler transport.Handler,
	unixListener net.Listener,
	tailnetListeners []net.Listener,
) error {
	connections := newConnectionGroup(handler)
	server := newWebSocketServer(ctx, normalized, connections)
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

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-fatal:
	}
	// Fatal listener failure is also daemon shutdown. Cancel the shared lifetime
	// before closing sockets or waiting, so handlers blocked in publication can
	// observe it and return.
	cancel()

	closeErr := unixListener.Close()
	closeErr = errors.Join(closeErr, connections.closeAll())
	if err := server.Shutdown(context.Background()); err != nil && !errors.Is(err, http.ErrServerClosed) {
		closeErr = errors.Join(closeErr, fmt.Errorf("daemon: close WebSocket server: %w", err))
	}
	listenerWG.Wait()
	server.wait()
	connections.wait()
	return errors.Join(runErr, closeErr)
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

	normalized := listenerConfig{
		stateDir:      filepath.Clean(cfg.StateDir),
		tailnetPort:   cfg.TailnetPort,
		webSocketPath: cfg.WebSocketPath,
		httpHandler:   cfg.HTTPHandler,
		reporter:      newErrorReporter(cfg.ReportError),
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

func validateWebSocketPath(value string) error {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || value == "" || value[0] != '/' || parsed.IsAbs() || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("daemon: WebSocket path %q must be an absolute path without a query or fragment", value)
	}
	if cleaned := path.Clean(value); cleaned != value {
		return fmt.Errorf("daemon: WebSocket path %q is not clean", value)
	}
	return nil
}

func listenTailnet(addrs []netip.Addr, port uint16) ([]net.Listener, []error) {
	listeners := make([]net.Listener, 0, len(addrs))
	var listenErrors []error
	service := strconv.Itoa(int(port))
	for _, addr := range addrs {
		network := "tcp6"
		if addr.Is4() {
			network = "tcp4"
		}
		endpoint := net.JoinHostPort(addr.String(), service)
		listener, err := net.Listen(network, endpoint)
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
	handlers sync.WaitGroup
}

func newWebSocketServer(ctx context.Context, cfg listenerConfig, connections *connectionGroup) *webSocketServer {
	server := &webSocketServer{}
	serveHTTP := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.handlers.Add(1)
		defer server.handlers.Done()
		if r.URL.EscapedPath() != cfg.webSocketPath {
			if cfg.httpHandler == nil {
				http.NotFound(w, r)
				return
			}
			cfg.httpHandler.ServeHTTP(w, r)
			return
		}
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{}, func(connectionCtx context.Context, conn transport.Conn) error {
			handlerCtx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(connectionCtx, cancel)
			defer func() {
				stop()
				cancel()
			}()
			return connections.handle(handlerCtx, conn)
		})
	})
	server.Server = &http.Server{
		Handler:           serveHTTP,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	return server
}

func (s *webSocketServer) wait() {
	s.handlers.Wait()
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
		connections.start(ctx, conn)
	}
}

type connectionGroup struct {
	handler transport.Handler
	mu      sync.Mutex
	nextID  uint64
	closed  bool
	conns   map[uint64]transport.Conn
	wg      sync.WaitGroup
}

func newConnectionGroup(handler transport.Handler) *connectionGroup {
	return &connectionGroup{handler: handler, conns: make(map[uint64]transport.Conn)}
}

func (g *connectionGroup) start(ctx context.Context, conn transport.Conn) {
	id, ok := g.add(conn)
	if !ok {
		_ = conn.Close()
		return
	}
	go func() {
		_ = g.run(id, ctx, conn)
	}()
}

func (g *connectionGroup) handle(ctx context.Context, conn transport.Conn) error {
	id, ok := g.add(conn)
	if !ok {
		_ = conn.Close()
		return transport.ErrClosed
	}
	return g.run(id, ctx, conn)
}

func (g *connectionGroup) add(conn transport.Conn) (uint64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return 0, false
	}
	g.nextID++
	id := g.nextID
	g.conns[id] = conn
	g.wg.Add(1)
	return id, true
}

func (g *connectionGroup) run(id uint64, ctx context.Context, conn transport.Conn) error {
	defer g.wg.Done()
	defer func() {
		g.mu.Lock()
		delete(g.conns, id)
		g.mu.Unlock()
		_ = conn.Close()
	}()
	return g.handler(ctx, conn)
}

func (g *connectionGroup) closeAll() error {
	g.mu.Lock()
	g.closed = true
	connections := make([]transport.Conn, 0, len(g.conns))
	for _, conn := range g.conns {
		connections = append(connections, conn)
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
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
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
	if err := removeStaleUnixSocket(socketPath); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("daemon: listen on Unix socket %s: %w", socketPath, err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("daemon: secure Unix socket %s: %w", socketPath, err)
	}
	// A replacement filesystem entry can reuse the unlinked socket's inode.
	// Stamp this entry so cleanup can distinguish that replacement even then.
	if err := os.Chtimes(socketPath, unixSocketOwnershipTime, unixSocketOwnershipTime); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("daemon: mark Unix socket %s: %w", socketPath, err)
	}
	info, err := os.Lstat(socketPath)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("daemon: inspect Unix socket %s: %w", socketPath, err)
	}
	return &ownedUnixListener{UnixListener: listener, path: socketPath, boundInfo: info}, nil
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
	mu sync.Mutex
	fn func(error)
}

func newErrorReporter(report func(error)) *errorReporter {
	if report == nil {
		report = func(err error) { log.Printf("%v", err) }
	}
	return &errorReporter{fn: report}
}

func (r *errorReporter) report(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fn(err)
}
