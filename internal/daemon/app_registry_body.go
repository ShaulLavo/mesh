package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

const minimumAppRegistryBodyBytesPerSecond int64 = 16 << 10

type appRegistryConnectionContextKey struct{}

func appRegistryConnectionContext(ctx context.Context, connection net.Conn) context.Context {
	return context.WithValue(ctx, appRegistryConnectionContextKey{}, connection)
}

func guardAppRegistryBody(handler http.Handler, idle time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			handler.ServeHTTP(w, r)
			return
		}
		guard := &appRegistryBodyGuard{
			ResponseWriter: w, body: r.Body, controller: http.NewResponseController(w),
			idle: idle,
		}
		r.Body = guard
		handler.ServeHTTP(guard, r)
		guard.mu.Lock()
		failed := guard.failed
		if !guard.hijacked {
			// Bound net/http's final drain after the response handler has finished.
			_ = guard.controller.SetReadDeadline(time.Now().Add(idle))
		}
		guard.mu.Unlock()
		if failed && r.ProtoMajor == 1 {
			// HTTP/2 deadlines belong to streams; closing the socket would kill other responses.
			// HTTP/1 otherwise lingers after a failed body read, retaining the slot.
			_ = guard.controller.Flush()
			if connection, ok := r.Context().Value(appRegistryConnectionContextKey{}).(net.Conn); ok {
				_ = connection.Close()
			}
		}
	})
}

type appRegistryBodyGuard struct {
	http.ResponseWriter
	body       io.ReadCloser
	controller *http.ResponseController
	mu         sync.Mutex
	waited     time.Duration
	idle       time.Duration
	bytes      int64
	responded  bool
	failed     bool
	hijacked   bool
}

func (g *appRegistryBodyGuard) Read(p []byte) (int, error) {
	g.mu.Lock()
	if !g.responded {
		budget := time.Duration(g.bytes/minimumAppRegistryBodyBytesPerSecond)*time.Second +
			time.Duration(g.bytes%minimumAppRegistryBodyBytesPerSecond)*time.Second/time.Duration(minimumAppRegistryBodyBytesPerSecond)
		remaining := min(g.idle, g.idle+budget-g.waited)
		_ = g.controller.SetReadDeadline(time.Now().Add(remaining))
	}
	g.mu.Unlock()
	readStarted := time.Now()
	n, err := g.body.Read(p)
	waited := time.Since(readStarted)
	g.mu.Lock()
	// Origin work and backpressure between reads are not client transfer time.
	g.waited += waited
	g.bytes += int64(n)
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		g.failed = true
	}
	g.mu.Unlock()
	if err != nil {
		return n, err //nolint:wrapcheck // preserve request-body timeout and EOF semantics
	}
	return n, nil
}

func (g *appRegistryBodyGuard) Close() error {
	if err := g.body.Close(); err != nil {
		return fmt.Errorf("app registry request body: %w", err)
	}
	return nil
}

func (g *appRegistryBodyGuard) responseStarted() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.responded {
		g.responded = true
		// A response must reach the client before net/http drains a stalled body.
		_ = g.controller.EnableFullDuplex()
		_ = g.controller.SetReadDeadline(time.Time{})
	}
}

func (g *appRegistryBodyGuard) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *appRegistryBodyGuard) Write(p []byte) (int, error) {
	g.responseStarted()
	n, err := g.ResponseWriter.Write(p)
	if err != nil {
		return n, fmt.Errorf("app registry response: %w", err)
	}
	return n, nil
}

func (g *appRegistryBodyGuard) WriteHeader(status int) {
	if status >= 200 || status == http.StatusSwitchingProtocols {
		g.responseStarted()
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *appRegistryBodyGuard) Flush() { g.responseStarted(); _ = g.controller.Flush() }

func (g *appRegistryBodyGuard) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	g.responseStarted()
	connection, reader, err := g.controller.Hijack()
	if err != nil {
		return nil, nil, fmt.Errorf("app registry response hijack: %w", err)
	}
	g.mu.Lock()
	g.hijacked = true
	g.mu.Unlock()
	return connection, reader, nil
}
