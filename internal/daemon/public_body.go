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

const minimumPublicBodyBytesPerSecond int64 = 16 << 10

type publicConnectionContextKey struct{}

func publicConnectionContext(ctx context.Context, connection net.Conn) context.Context {
	return context.WithValue(ctx, publicConnectionContextKey{}, connection)
}

func guardPublicBody(handler http.Handler, idle time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			handler.ServeHTTP(w, r)
			return
		}
		guard := &publicBodyGuard{
			ResponseWriter: w, body: r.Body, controller: http.NewResponseController(w),
			started: time.Now(), idle: idle,
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
			if connection, ok := r.Context().Value(publicConnectionContextKey{}).(net.Conn); ok {
				_ = connection.Close()
			}
		}
	})
}

type publicBodyGuard struct {
	http.ResponseWriter
	body       io.ReadCloser
	controller *http.ResponseController
	mu         sync.Mutex
	started    time.Time
	idle       time.Duration
	bytes      int64
	responded  bool
	failed     bool
	hijacked   bool
}

func (g *publicBodyGuard) Read(p []byte) (int, error) {
	g.mu.Lock()
	if !g.responded {
		budget := time.Duration(g.bytes/minimumPublicBodyBytesPerSecond)*time.Second +
			time.Duration(g.bytes%minimumPublicBodyBytesPerSecond)*time.Second/time.Duration(minimumPublicBodyBytesPerSecond)
		deadline := g.started.Add(g.idle + budget)
		if idleDeadline := time.Now().Add(g.idle); idleDeadline.Before(deadline) {
			deadline = idleDeadline
		}
		_ = g.controller.SetReadDeadline(deadline)
	}
	g.mu.Unlock()
	n, err := g.body.Read(p)
	g.mu.Lock()
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

func (g *publicBodyGuard) Close() error {
	if err := g.body.Close(); err != nil {
		return fmt.Errorf("public request body: %w", err)
	}
	return nil
}

func (g *publicBodyGuard) responseStarted() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.responded {
		g.responded = true
		// A response must reach the client before net/http drains a stalled body.
		_ = g.controller.EnableFullDuplex()
		_ = g.controller.SetReadDeadline(time.Time{})
	}
}

func (g *publicBodyGuard) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *publicBodyGuard) Write(p []byte) (int, error) {
	g.responseStarted()
	n, err := g.ResponseWriter.Write(p)
	if err != nil {
		return n, fmt.Errorf("public response: %w", err)
	}
	return n, nil
}

func (g *publicBodyGuard) WriteHeader(status int) {
	if status >= 200 || status == http.StatusSwitchingProtocols {
		g.responseStarted()
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *publicBodyGuard) Flush() { g.responseStarted(); _ = g.controller.Flush() }

func (g *publicBodyGuard) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	g.responseStarted()
	connection, reader, err := g.controller.Hijack()
	if err != nil {
		return nil, nil, fmt.Errorf("public response hijack: %w", err)
	}
	g.mu.Lock()
	g.hijacked = true
	g.mu.Unlock()
	return connection, reader, nil
}
