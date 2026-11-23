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

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/closelog"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transporttrace"
)

const (
	protocolHeaderSize    = 5
	inboundQueueCapacity  = 256
	inboundQueueByteLimit = 8 << 20
)

// InboundByteBudget lets a frontend account for a connection's inbound receive
// queue inside its shared memory budget (PLAN section 2.5).
type InboundByteBudget interface {
	ReserveBytes(n int) bool
	ReleaseBytes(n int)
}

// AttachInboundBudget binds conn's inbound receive queue to budget when conn
// uses a background receive buffer. Existing queued bytes are charged immediately.
func AttachInboundBudget(conn Conn, budget InboundByteBudget) bool {
	sc, ok := conn.(*socketConn)
	if !ok || budget == nil {
		return true
	}
	return sc.attachInboundBudget(budget)
}

type inboundFrame struct {
	frame  protocol.Frame
	bytes  int
	budget InboundByteBudget
}

type socketConn struct {
	ws           *websocket.Conn
	stream       io.ReadWriteCloser
	writeMu      sync.Mutex
	writeCurrent func() bool
	ctx          context.Context
	cancel       context.CancelFunc
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
	budget     InboundByteBudget
	trace      *transporttrace.Connection
}

func newSocketConn(ws *websocket.Conn, keepAlive KeepAlive) *socketConn {
	return newSecureSocketConn(ws, nil, keepAlive)
}

func newSecureSocketConn(ws *websocket.Conn, stream io.ReadWriteCloser, keepAlive KeepAlive, observers ...*transporttrace.Connection) *socketConn {
	ctx, cancel := context.WithCancel(context.Background())
	writeCtx, cancelWrite := context.WithCancel(ctx)
	c := &socketConn{
		ws:          ws,
		stream:      stream,
		ctx:         ctx,
		cancel:      cancel,
		writeCtx:    writeCtx,
		cancelWrite: cancelWrite,
		frames:      make(chan inboundFrame, inboundQueueCapacity),
		done:        make(chan struct{}),
	}
	if len(observers) != 0 {
		c.trace = observers[0]
	}
	c.trace.Emit(ctx, transporttrace.StreamBegin, nil)
	// A WebSocket message contains the five-byte Mesh header plus a payload
	// already bounded by protocol.MaxPayload.
	ws.SetReadLimit(protocol.MaxPayload + protocolHeaderSize)
	go c.readLoop()
	go c.keepAliveLoop(keepAlive)
	return c
}

func (c *socketConn) attachInboundBudget(budget InboundByteBudget) bool {
	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	if c.queueBytes > 0 && !budget.ReserveBytes(c.queueBytes) {
		return false
	}
	c.budget = budget
	return true
}

func (c *socketConn) releaseInboundFrame(inbound inboundFrame) {
	c.queueMu.Lock()
	c.queueBytes -= inbound.bytes
	budget := inbound.budget
	if budget == nil {
		budget = c.budget
	}
	c.queueMu.Unlock()
	if budget != nil && inbound.bytes > 0 {
		budget.ReleaseBytes(inbound.bytes)
	}
}

func (c *socketConn) drainUnreadInbound() {
	for {
		select {
		case inbound, ok := <-c.frames:
			if !ok {
				return
			}
			c.releaseInboundFrame(inbound)
		default:
			return
		}
	}
}

func (c *socketConn) ReadFrame() (protocol.Frame, error) {
	inbound, ok := <-c.frames
	if ok {
		c.releaseInboundFrame(inbound)
		c.observeSession(inbound.frame, protocol.TypeAttach, transporttrace.AttachRead)
		return inbound.frame, nil
	}
	err := c.terminalError()
	c.trace.Emit(c.ctx, transporttrace.StreamReadEnd, err)
	return protocol.Frame{}, err
}

func (c *socketConn) WriteFrame(frame protocol.Frame) error {
	return c.write(frame, nil)
}

func (c *socketConn) writeAdmittedControl(frame protocol.Frame, admit func() error) error {
	return c.write(frame, admit)
}

func (c *socketConn) write(frame protocol.Frame, admit func() error) error {
	payload, err := encodeFrame(frame)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeCurrent != nil && !c.writeCurrent() {
		err := errors.New("transport: scoped grant no longer current")
		c.fail(err)
		return err
	}
	if admit != nil {
		if err := admit(); err != nil {
			return err
		}
	}
	var writeErr error
	if c.stream != nil {
		_, writeErr = c.stream.Write(payload)
	} else {
		writeErr = c.ws.Write(c.writeCtx, websocket.MessageBinary, payload)
	}
	if err := writeErr; err != nil {
		c.trace.Emit(c.ctx, transporttrace.StreamWriteEnd, err)
		wrapped := fmt.Errorf("transport: write WebSocket frame: %w", err)
		c.fail(wrapped)
		return wrapped
	}
	c.observeSession(frame, protocol.TypeAttached, transporttrace.AttachedWrite)
	return nil
}

func (c *socketConn) observeSession(frame protocol.Frame, kind string, stage transporttrace.Stage) {
	if c.trace == nil || frame.Kind != protocol.KindControl {
		return
	}
	control, err := protocol.DecodeControl(frame.Payload)
	if err == nil && control.Type == kind {
		c.trace.Session(stage, control.SessionID)
	}
}

func (c *socketConn) Close() error {
	c.trace.Emit(c.ctx, transporttrace.StreamCloseBegin, nil)
	won := c.setCause(ErrClosed)
	if !won {
		<-c.done
		c.drainUnreadInbound()
		c.trace.Emit(c.ctx, transporttrace.StreamCloseEnd, nil)
		return nil
	}

	c.cancelWrite()
	err := c.ws.Close(websocket.StatusNormalClosure, "")
	c.cancel()
	<-c.done
	c.drainUnreadInbound()
	c.trace.Emit(c.ctx, transporttrace.StreamCloseEnd, err)
	if !closeCompleted(err) {
		return fmt.Errorf("transport: close WebSocket: %w", err)
	}
	return nil
}

// closeCompleted reports whether a close error means the connection is already
// gone rather than that closing failed. EOF can also mean a truncated frame,
// so it remains a protocol failure even during the close handshake.
func closeCompleted(err error) bool {
	return err == nil || errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed)
}

type frameByteReader struct {
	io.Reader
	bytes int
}

func (r *frameByteReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytes += n
	return n, err //nolint:wrapcheck // protocol.Reader needs the underlying EOF and stream errors
}

func (c *socketConn) readLoop() {
	defer close(c.done)
	defer close(c.frames)

	var reader *protocol.Reader
	var counted *frameByteReader
	if c.stream != nil {
		counted = &frameByteReader{Reader: c.stream}
		reader = protocol.NewReader(counted)
	}
	for {
		frame, payloadBytes, err := c.readFrame(reader, counted)
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
		budget := c.budget
		if payloadBytes > inboundQueueByteLimit-c.queueBytes || (budget != nil && !budget.ReserveBytes(payloadBytes)) {
			c.queueMu.Unlock()
			c.fail(ErrInboundQueueFull)
			return
		}
		c.queueBytes += payloadBytes
		select {
		case c.frames <- inboundFrame{frame: frame, bytes: payloadBytes, budget: budget}:
			c.queueMu.Unlock()
		default:
			c.queueBytes -= payloadBytes
			c.queueMu.Unlock()
			if budget != nil {
				budget.ReleaseBytes(payloadBytes)
			}
			c.fail(ErrInboundQueueFull)
			return
		}
	}
}

func (c *socketConn) readFrame(reader *protocol.Reader, counted *frameByteReader) (protocol.Frame, int, error) {
	if reader != nil {
		counted.bytes = 0
		frame, err := reader.ReadFrame()
		if err != nil {
			return frame, counted.bytes, fmt.Errorf("transport: read encrypted Mesh frame: %w", err)
		}
		return frame, counted.bytes, nil
	}
	messageType, payload, err := c.ws.Read(c.ctx)
	if err != nil {
		return protocol.Frame{}, 0, fmt.Errorf("transport: read WebSocket frame: %w", err)
	}
	if messageType != websocket.MessageBinary {
		return protocol.Frame{}, 0, fmt.Errorf("%w: expected binary WebSocket message, got %s", ErrInvalidFrame, messageType)
	}
	frame, err := decodeFrame(payload)
	return frame, len(payload), err
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
	c.trace.Emit(c.ctx, transporttrace.StreamFailed, err)
	if !c.setCause(err) {
		return
	}
	status := websocket.CloseStatus(err)
	if status != websocket.StatusNormalClosure && status != websocket.StatusGoingAway {
		closelog.Printf("transport: stage=established-stream classification=link-failed error=%q", err)
	}
	c.cancel()
	c.trace.Emit(c.ctx, transporttrace.StreamAbortBegin, nil)
	closeErr := c.ws.CloseNow()
	c.trace.Emit(c.ctx, transporttrace.StreamAbortEnd, closeErr)
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

// Stopped reports whether conn already knows it is dead: its reader saw the
// peer close, the link failed, the grant ended, or a close began. A caller
// that must not write onto a dead connection (a request that can never be
// retried) checks it first. Connections without a reader of their own
// report false.
func Stopped(conn Conn) bool {
	stoppable, ok := conn.(interface{ ending() bool })
	return ok && stoppable.ending()
}

// Abort drops conn at once. Close waits for the peer's half of the close
// handshake, which a silent peer never sends; a caller giving up on a
// connection it will discard uses Abort instead.
func Abort(conn Conn) error {
	if abortable, ok := conn.(interface{ Abort() error }); ok {
		if err := abortable.Abort(); err != nil {
			return fmt.Errorf("transport: abort connection: %w", err)
		}
		return nil
	}
	return conn.Close() //nolint:wrapcheck // the connection's own close error
}

// Abort closes the socket without the close handshake.
func (c *socketConn) Abort() error {
	if !c.setCause(ErrClosed) {
		return nil
	}
	c.cancelWrite()
	c.cancel()
	_ = c.ws.CloseNow()
	return nil
}

// ending reports a recorded terminal cause, which fail and Close set before
// the reader finishes.
func (c *socketConn) ending() bool {
	c.causeMu.Lock()
	defer c.causeMu.Unlock()
	return c.cause != nil || c.stopped()
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
func ServeWithOptions(w http.ResponseWriter, r *http.Request, opts ServeOptions, h Handler) (result error) {
	defer observeServeResult(r.Context(), &result)
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
	conn, peer, err := acceptSocket(w, r, opts, keepAlive)
	if err != nil {
		return err
	}
	batched := &BatchingConn{dst: conn, opts: batchOpts}
	handlerContext := context.WithoutCancel(r.Context())
	if opts.Auth != nil {
		handlerContext = context.WithValue(handlerContext, peerKey{}, peer)
	}
	ctx, cancel := context.WithCancel(handlerContext)
	defer cancel()
	go func() {
		select {
		case <-conn.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	var handlerConn Conn = batched
	if opts.Auth != nil {
		handlerConn = &authorizedConn{Conn: batched, grant: peer.grant}
		go conn.watchAuthorization(peer.grant.Current)
	}
	handlerErr := h(ctx, handlerConn)
	transporttrace.Emit(r.Context(), transporttrace.HandlerEnd, handlerErr)
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

func observeServeResult(ctx context.Context, result *error) {
	transporttrace.Emit(ctx, transporttrace.ServeEnd, *result)
}

func acceptSocket(w http.ResponseWriter, r *http.Request, opts ServeOptions, keepAlive KeepAlive) (*socketConn, AuthenticatedPeer, error) {
	var subprotocols []string
	if opts.Auth != nil {
		if err := opts.Auth.validate(true); err != nil {
			return nil, AuthenticatedPeer{}, err
		}
		if !containsAuthProtocol(r) {
			w.Header().Set(ControlAuthenticationHeader, AuthProtocol)
			http.Error(w, "Mesh control authentication required", http.StatusUpgradeRequired)
			return nil, AuthenticatedPeer{}, ErrAuthenticationRequired
		}
		subprotocols = []string{AuthProtocol}
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    subprotocols,
		CompressionMode: websocket.CompressionDisabled,
		OriginPatterns:  append([]string(nil), opts.OriginPatterns...),
	})
	transporttrace.Emit(r.Context(), transporttrace.UpgradeDone, err)
	if err != nil {
		return nil, AuthenticatedPeer{}, fmt.Errorf("transport: accept WebSocket: %w", err)
	}
	if opts.Auth == nil {
		return newSocketConn(ws, keepAlive), AuthenticatedPeer{}, nil //nolint:contextcheck // sockets own their lifetime after the HTTP upgrade
	}
	secure, peer, err := authenticate(r.Context(), ws, opts.Auth, true)
	if err != nil {
		return nil, AuthenticatedPeer{}, err
	}
	conn := newSecureSocketConn(ws, secure, keepAlive, transporttrace.From(r.Context())) //nolint:contextcheck // sockets own their lifetime after the HTTP upgrade
	if peer.grant.Class == access.Scoped {
		conn.writeCurrent = peer.grant.Current
	}
	return conn, peer, nil
}
