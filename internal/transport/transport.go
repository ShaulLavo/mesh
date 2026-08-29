// Package transport carries Mesh protocol frames over WebSockets.
package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

const (
	defaultKeepAliveInterval = 15 * time.Second
	defaultKeepAliveTimeout  = 5 * time.Second
	defaultBackoffInitial    = 100 * time.Millisecond
	defaultBackoffMax        = 5 * time.Second
	defaultBackoffJitter     = 0.2
)

var (
	// ErrClosed reports an operation on a connection closed by its caller.
	ErrClosed = errors.New("transport: closed")
	// ErrInboundQueueFull reports a peer that sends faster than its caller can
	// consume. Closing is safer than blocking WebSocket pong processing.
	ErrInboundQueueFull = errors.New("transport: inbound frame queue full")
	// ErrInvalidFrame reports a WebSocket message that is not one complete Mesh
	// protocol frame. Reconnecting cannot repair a peer protocol violation.
	ErrInvalidFrame = errors.New("transport: invalid frame")
)

// Conn carries complete Mesh protocol frames. A Conn permits one concurrent
// reader and any number of concurrent writers.
type Conn interface {
	ReadFrame() (protocol.Frame, error)
	WriteFrame(protocol.Frame) error
	Close() error
}

// Handler handles one accepted WebSocket connection.
type Handler func(context.Context, Conn) error

// KeepAlive controls active WebSocket health checks.
type KeepAlive struct {
	Interval time.Duration
	Timeout  time.Duration
}

// Backoff controls reconnect delay. Jitter is a fraction between zero and one.
// A zero Backoff uses the package defaults.
type Backoff struct {
	Initial time.Duration
	Max     time.Duration
	Jitter  float64
}

// DialOptions configures a reconnecting client connection.
type DialOptions struct {
	HTTPClient *http.Client
	HTTPHeader http.Header
	KeepAlive  KeepAlive
	Backoff    Backoff
}

// ServeOptions configures an accepted connection.
type ServeOptions struct {
	KeepAlive      KeepAlive
	Batch          BatchOptions
	OriginPatterns []string
}

type connectionOptions struct {
	keepAlive KeepAlive
	backoff   Backoff
}

func normalizeDialOptions(opts DialOptions) (connectionOptions, error) {
	keepAlive, err := normalizeKeepAlive(opts.KeepAlive)
	if err != nil {
		return connectionOptions{}, err
	}
	backoff, err := normalizeBackoff(opts.Backoff)
	if err != nil {
		return connectionOptions{}, err
	}
	return connectionOptions{keepAlive: keepAlive, backoff: backoff}, nil
}

func normalizeKeepAlive(v KeepAlive) (KeepAlive, error) {
	if v == (KeepAlive{}) {
		return KeepAlive{Interval: defaultKeepAliveInterval, Timeout: defaultKeepAliveTimeout}, nil
	}
	if v.Interval <= 0 {
		return KeepAlive{}, fmt.Errorf("transport: keepalive interval must be positive")
	}
	if v.Timeout <= 0 {
		return KeepAlive{}, fmt.Errorf("transport: keepalive timeout must be positive")
	}
	return v, nil
}

func normalizeBackoff(v Backoff) (Backoff, error) {
	if v == (Backoff{}) {
		return Backoff{Initial: defaultBackoffInitial, Max: defaultBackoffMax, Jitter: defaultBackoffJitter}, nil
	}
	if v.Initial <= 0 {
		return Backoff{}, fmt.Errorf("transport: backoff initial delay must be positive")
	}
	if v.Max < v.Initial {
		return Backoff{}, fmt.Errorf("transport: backoff maximum must be at least the initial delay")
	}
	if v.Jitter < 0 || v.Jitter > 1 {
		return Backoff{}, fmt.Errorf("transport: backoff jitter must be between zero and one")
	}
	return v, nil
}
