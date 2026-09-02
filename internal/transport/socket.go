package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/shaul/mesh/internal/protocol"
)

const (
	protocolHeaderSize    = 5
	inboundQueueCapacity  = 256
	inboundQueueByteLimit = 8 << 20
)

type inboundFrame struct {
	frame protocol.Frame
	bytes int
}

type socketConn struct {
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	// The close handshake still needs the reader, so writes have their own
	// cancellation path.
	writeCtx    context.Context
	cancelWrite context.CancelFunc
	frames      chan inboundFrame
	done        chan struct{}

	causeOnce  sync.Once
	causeMu    sync.Mutex
	cause      error
	queueMu    sync.Mutex
	queueBytes int
}

func newSocketConn(ws *websocket.Conn, keepAlive KeepAlive) *socketConn {
	ctx, cancel := context.WithCancel(context.Background())
	writeCtx, cancelWrite := context.WithCancel(ctx)
	c := &socketConn{
		ws:          ws,
		ctx:         ctx,
		cancel:      cancel,
		writeCtx:    writeCtx,
		cancelWrite: cancelWrite,
		frames:      make(chan inboundFrame, inboundQueueCapacity),
		done:        make(chan struct{}),
	}
	// A WebSocket message contains the five-byte Mesh header plus a payload
	// already bounded by protocol.MaxPayload.
	ws.SetReadLimit(protocol.MaxPayload + protocolHeaderSize)
	go c.readLoop()
	go c.keepAliveLoop(keepAlive)
	return c
}

func (c *socketConn) ReadFrame() (protocol.Frame, error) {
	inbound, ok := <-c.frames
	if ok {
		c.queueMu.Lock()
		c.queueBytes -= inbound.bytes
		c.queueMu.Unlock()
		return inbound.frame, nil
	}
	return protocol.Frame{}, c.terminalError()
}

func (c *socketConn) WriteFrame(frame protocol.Frame) error {
	payload, err := encodeFrame(frame)
	if err != nil {
		return err
	}
	if err := c.ws.Write(c.writeCtx, websocket.MessageBinary, payload); err != nil {
		wrapped := fmt.Errorf("transport: write WebSocket frame: %w", err)
		c.fail(wrapped)
		return wrapped
	}
	return nil
}

func (c *socketConn) Close() error {
	won := c.setCause(ErrClosed)
	if !won {
		<-c.done
		return nil
	}

	c.cancelWrite()
	err := c.ws.Close(websocket.StatusNormalClosure, "")
	c.cancel()
	<-c.done
	if !closeCompleted(err) {
		return fmt.Errorf("transport: close WebSocket: %w", err)
	}
	return nil
}

// closeCompleted reports whether a close error means the connection is already
// gone rather than that closing failed. A peer that closed first has torn the
// transport down, so our own close lands on a dead socket and returns
// net.ErrClosed; the connection is closed either way.
func closeCompleted(err error) bool {
	return err == nil || errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed)
}

func (c *socketConn) readLoop() {
	defer close(c.done)
	defer close(c.frames)

	for {
		messageType, payload, err := c.ws.Read(c.ctx)
		if err != nil {
			c.fail(fmt.Errorf("transport: read WebSocket frame: %w", err))
			return
		}
		if messageType != websocket.MessageBinary {
			c.fail(fmt.Errorf("%w: expected binary WebSocket message, got %s", ErrInvalidFrame, messageType))
			return
		}
		frame, err := decodeFrame(payload)
		if err != nil {
			c.fail(err)
			return
		}

		select {
		case <-c.ctx.Done():
			return
		default:
		}
		c.queueMu.Lock()
		if len(payload) > inboundQueueByteLimit-c.queueBytes {
			c.queueMu.Unlock()
			c.fail(ErrInboundQueueFull)
			return
		}
		c.queueBytes += len(payload)
		select {
		case c.frames <- inboundFrame{frame: frame, bytes: len(payload)}:
			c.queueMu.Unlock()
		default:
			c.queueBytes -= len(payload)
			c.queueMu.Unlock()
			c.fail(ErrInboundQueueFull)
			return
		}
	}
}

func (c *socketConn) keepAliveLoop(opts KeepAlive) {
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(c.ctx, opts.Timeout)
			err := c.ws.Ping(ctx)
			cancel()
			if err != nil {
				c.fail(fmt.Errorf("transport: keepalive: %w", err))
				return
			}
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *socketConn) fail(err error) {
	if !c.setCause(err) {
		return
	}
	c.cancel()
	_ = c.ws.CloseNow()
}

func (c *socketConn) setCause(err error) bool {
	won := false
	c.causeOnce.Do(func() {
		won = true
		c.causeMu.Lock()
		c.cause = err
		c.causeMu.Unlock()
	})
	return won
}

func (c *socketConn) terminalError() error {
	c.causeMu.Lock()
	defer c.causeMu.Unlock()
	if c.cause == nil {
		return io.EOF
	}
	return c.cause
}

func (c *socketConn) stopped() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func encodeFrame(frame protocol.Frame) ([]byte, error) {
	var buf bytes.Buffer
	w := protocol.NewWriter(&buf)
	var err error
	switch frame.Kind {
	case protocol.KindControl:
		err = w.WriteControl(frame.Payload)
	case protocol.KindData:
		err = w.WriteData(frame.Session, frame.Seq, frame.Payload)
	default:
		if frame.Kind == 0 {
			return nil, fmt.Errorf("transport: invalid protocol frame kind 0")
		}
		// Every non-control frame without a sequence uses session+payload on
		// the wire. WriteInput owns that layout; replacing only its kind byte
		// also supports additive session-scoped frame kinds.
		err = w.WriteInput(frame.Session, frame.Payload)
		if err == nil {
			buf.Bytes()[0] = byte(frame.Kind)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("transport: encode protocol frame: %w", err)
	}
	return buf.Bytes(), nil
}

func decodeFrame(payload []byte) (protocol.Frame, error) {
	r := bytes.NewReader(payload)
	frame, err := protocol.NewReader(r).ReadFrame()
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("%w: decode protocol frame: %w", ErrInvalidFrame, err)
	}
	if r.Len() != 0 {
		return protocol.Frame{}, fmt.Errorf("%w: WebSocket message contains %d trailing bytes", ErrInvalidFrame, r.Len())
	}
	return frame, nil
}

// Serve accepts one WebSocket and passes it to h.
func Serve(w http.ResponseWriter, r *http.Request, h Handler) error {
	return ServeWithOptions(w, r, ServeOptions{}, h)
}

// ServeWithOptions accepts one WebSocket with explicit health and origin policy.
func ServeWithOptions(w http.ResponseWriter, r *http.Request, opts ServeOptions, h Handler) error {
	if h == nil {
		return fmt.Errorf("transport: nil WebSocket handler")
	}
	keepAlive, err := normalizeKeepAlive(opts.KeepAlive)
	if err != nil {
		return err
	}
	batchOpts, err := normalizeBatchOptions(opts.Batch)
	if err != nil {
		return err
	}
	// A Mesh protocol client is never a browser, so an Origin header means a
	// page is dialling the control socket. The default same-origin check is no
	// defence here: served services share this host and port, so a file out of
	// a `mesh serve files` root satisfies it and can open a session. Refuse the
	// whole class rather than enumerating origins. Non-browser clients send no
	// Origin and are unaffected, and OriginPatterns still governs any caller
	// that deliberately opts a browser origin in.
	if len(opts.OriginPatterns) == 0 && r.Header.Get("Origin") != "" {
		http.Error(w, "browser origins cannot open a Mesh control connection", http.StatusForbidden)
		return fmt.Errorf("transport: refused WebSocket from browser origin %q", r.Header.Get("Origin"))
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
		OriginPatterns:  append([]string(nil), opts.OriginPatterns...),
	})
	if err != nil {
		return fmt.Errorf("transport: accept WebSocket: %w", err)
	}
	conn := newSocketConn(ws, keepAlive)
	batched := &BatchingConn{dst: conn, opts: batchOpts}
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	go func() {
		select {
		case <-conn.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	handlerErr := h(ctx, batched)
	flushErr := batched.Flush()
	closeErr := batched.Close()
	if handlerErr != nil {
		return handlerErr
	}
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}
