package daemon

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"time"

	meshserve "github.com/shaul/mesh/internal/serve"
)

const (
	// demandListenerIdleTimeout closes a keep-alive connection nobody is
	// using, so an open browser tab without a live socket lets the route idle.
	demandListenerIdleTimeout = 2 * time.Minute
)

func bindLoopback(port uint16) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
}

// errPortStillHeld is a retry that found the port taken again, when naming
// the holder was not asked for.
var errPortStillHeld = errors.New("port still held")

func (m *demandManager) listenLocked(owner, route string, port uint16, describe bool) (*demandListener, error) {
	raw, err := m.bind(port)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			if !describe {
				return nil, errPortStillHeld
			}
			return nil, fmt.Errorf("port %d is held by %s", port, m.holder(port))
		}
		return nil, fmt.Errorf("listen on 127.0.0.1:%d: %w", port, err)
	}
	listener := &demandListener{manager: m, owner: owner, route: route, port: port}
	listener.server = &http.Server{
		Handler:           http.HandlerFunc(listener.serveHTTP),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       demandListenerIdleTimeout,
	}
	listener.listener = &countingListener{Listener: raw, open: map[*countedConn]struct{}{}, hold: func() func() {
		if route := m.route(owner); route != nil {
			release, _ := route.hold()
			return release
		}
		return func() {}
	}}
	go func() {
		if err := listener.server.Serve(listener.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.report(fmt.Errorf("daemon: route %s listener on port %d: %w", route, port, err))
		}
	}()
	return listener, nil
}

// demandListener serves one loopback port for one route.
type demandListener struct {
	manager  *demandManager
	owner    string
	route    string
	port     uint16
	listener *countingListener
	server   *http.Server
	handlers sync.Map // upstream port and isolation → proxy handler
}

func (l *demandListener) close() {
	_ = l.server.Close()
	l.listener.closeAll()
}

func (l *demandListener) serveHTTP(w http.ResponseWriter, request *http.Request) {
	route := l.manager.route(l.owner)
	if route == nil {
		http.Error(w, "route removed", http.StatusNotFound)
		return
	}
	if err := route.ready(request.Context()); err != nil {
		meshserve.WriteDemandFailure(w, err, l.manager.logger)
		return
	}
	service := route.definition()
	upstream := ""
	for _, listen := range service.Listens {
		if listen.Public == l.port {
			upstream = strconv.Itoa(int(listen.Upstream))
		}
	}
	if upstream == "" {
		http.Error(w, "listener removed", http.StatusNotFound)
		return
	}
	key := upstream
	if service.Isolate {
		key += "+isolate"
	}
	handler, ok := l.handlers.Load(key)
	if !ok {
		built, err := meshserve.Handler(meshserve.Service{
			Name: service.Name, Kind: meshserve.Proxy, Target: upstream, Isolate: service.Isolate,
		}, "/")
		if err != nil {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		handler, _ = l.handlers.LoadOrStore(key, built)
	}
	handler.(http.Handler).ServeHTTP(w, request)
}

// countingListener counts each accepted connection against its route until
// the connection closes. Keep-alives and upgraded WebSockets alike hold the
// route open; the server's idle timeout ends keep-alives nobody uses. It also
// owns every connection it accepted, because http.Server.Close forgets one
// once a proxied upgrade hijacks it.
type countingListener struct {
	net.Listener
	hold func() func()

	mu     sync.Mutex
	closed bool
	open   map[*countedConn]struct{}
}

func (l *countingListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	counted := &countedConn{Conn: connection, owner: l, release: l.hold()}
	l.mu.Lock()
	closed := l.closed
	if !closed {
		l.open[counted] = struct{}{}
	}
	l.mu.Unlock()
	if closed {
		_ = counted.Close()
		return nil, net.ErrClosed
	}
	return counted, nil
}

// closeAll closes every connection still open and refuses any accepted later.
func (l *countingListener) closeAll() {
	l.mu.Lock()
	l.closed = true
	open := make([]*countedConn, 0, len(l.open))
	for connection := range l.open {
		open = append(open, connection)
	}
	l.mu.Unlock()
	for _, connection := range open {
		_ = connection.Close()
	}
}

type countedConn struct {
	net.Conn
	owner   *countingListener
	once    sync.Once
	release func()
}

func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.owner.mu.Lock()
		delete(c.owner.open, c)
		c.owner.mu.Unlock()
		c.release()
	})
	return err
}
