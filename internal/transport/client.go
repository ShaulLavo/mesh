package transport

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/shaul/mesh/internal/protocol"
)

type resumeState struct {
	attach   protocol.Control
	next     uint64
	haveNext bool
}

type reconnectingConn struct {
	url       string
	dialOpts  websocket.DialOptions
	keepAlive KeepAlive
	backoff   Backoff
	ctx       context.Context
	cancel    context.CancelFunc

	readMu      sync.Mutex
	writeMu     sync.Mutex
	reconnectMu sync.Mutex
	stateMu     sync.Mutex
	current     *socketConn
	closed      bool
	resumes     map[string]resumeState
}

// Dial opens a reconnecting WebSocket connection. After a link failure, the
// next read or write reconnects and sends one session.attach per tracked
// session at the next byte not yet returned by ReadFrame.
func Dial(ctx context.Context, url string, opts DialOptions) (Conn, error) {
	normalized, err := normalizeDialOptions(opts)
	if err != nil {
		return nil, err
	}
	dialOpts := websocket.DialOptions{
		HTTPClient:      opts.HTTPClient,
		HTTPHeader:      cloneHeader(opts.HTTPHeader),
		CompressionMode: websocket.CompressionDisabled,
	}
	initial, err := dialSocket(ctx, url, dialOpts, normalized.keepAlive)
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &reconnectingConn{
		url:       url,
		dialOpts:  dialOpts,
		keepAlive: normalized.keepAlive,
		backoff:   normalized.backoff,
		ctx:       lifetime,
		cancel:    cancel,
		current:   initial,
		resumes:   make(map[string]resumeState),
	}, nil
}

func dialSocket(ctx context.Context, url string, opts websocket.DialOptions, keepAlive KeepAlive) (*socketConn, error) {
	ws, response, err := websocket.Dial(ctx, url, &opts)
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("transport: dial %s: HTTP %s: %w", url, response.Status, err)
		}
		return nil, fmt.Errorf("transport: dial %s: %w", url, err)
	}
	return newSocketConn(ws, keepAlive), nil
}

func (c *reconnectingConn) ReadFrame() (protocol.Frame, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	for {
		conn, err := c.connection(nil)
		if err != nil {
			return protocol.Frame{}, err
		}
		frame, err := conn.ReadFrame()
		if err != nil {
			c.invalidate(conn)
			if !retryableReadError(err) {
				return protocol.Frame{}, err
			}
			if _, reconnectErr := c.connection(conn); reconnectErr != nil {
				return protocol.Frame{}, reconnectErr
			}
			continue
		}
		frame, drop, err := c.observeFrame(frame)
		if err != nil {
			conn.fail(err)
			c.invalidate(conn)
			return protocol.Frame{}, err
		}
		if drop {
			continue
		}
		return frame, nil
	}
}

func (c *reconnectingConn) WriteFrame(frame protocol.Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	conn, err := c.connection(nil)
	if err != nil {
		return err
	}
	if conn.stopped() {
		c.invalidate(conn)
		conn, err = c.connection(conn)
		if err != nil {
			return err
		}
	}
	if err := conn.WriteFrame(frame); err != nil {
		c.invalidate(conn)
		return err
	}
	c.observeWrite(frame)
	return nil
}

func (c *reconnectingConn) Close() error {
	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return nil
	}
	c.closed = true
	conn := c.current
	c.current = nil
	c.stateMu.Unlock()
	c.cancel()

	var err error
	if conn != nil {
		err = conn.Close()
	}
	// A concurrent reconnect observes the cancelled context and exits before
	// Close returns.
	c.reconnectMu.Lock()
	c.reconnectMu.Unlock()
	return err
}

func (c *reconnectingConn) connection(failed *socketConn) (*socketConn, error) {
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()

	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return nil, ErrClosed
	}
	if c.current != nil && c.current != failed {
		conn := c.current
		c.stateMu.Unlock()
		return conn, nil
	}
	if c.current == failed {
		c.current = nil
	}
	c.stateMu.Unlock()

	for attempt := 0; ; attempt++ {
		conn, err := dialSocket(c.ctx, c.url, c.dialOpts, c.keepAlive)
		if err == nil {
			err = c.sendResumes(conn)
		}
		if err == nil {
			c.stateMu.Lock()
			if c.closed {
				c.stateMu.Unlock()
				_ = conn.Close()
				return nil, ErrClosed
			}
			c.current = conn
			c.stateMu.Unlock()
			return conn, nil
		}
		if conn != nil {
			_ = conn.Close()
		}

		delay := backoffDelay(attempt, c.backoff, rand.Float64())
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-c.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, ErrClosed
		}
	}
}

func (c *reconnectingConn) invalidate(conn *socketConn) {
	c.stateMu.Lock()
	if c.current == conn {
		c.current = nil
	}
	c.stateMu.Unlock()
}

func (c *reconnectingConn) observeWrite(frame protocol.Frame) {
	if frame.Kind != protocol.KindControl {
		return
	}
	msg, err := protocol.DecodeControl(frame.Payload)
	if err != nil || msg.SessionID == "" {
		return
	}
	switch msg.Type {
	case protocol.TypeAttach:
		state := resumeState{attach: msg}
		if msg.LastSeq != nil {
			state.next = *msg.LastSeq
			state.haveNext = true
		}
		c.stateMu.Lock()
		c.resumes[msg.SessionID] = state
		c.stateMu.Unlock()
	case protocol.TypeDetach:
		c.stateMu.Lock()
		delete(c.resumes, msg.SessionID)
		c.stateMu.Unlock()
	}
}

func (c *reconnectingConn) observeFrame(frame protocol.Frame) (protocol.Frame, bool, error) {
	switch frame.Kind {
	case protocol.KindControl:
		msg, err := protocol.DecodeControl(frame.Payload)
		if err != nil || msg.SessionID == "" {
			return frame, false, nil
		}
		c.stateMu.Lock()
		state, tracked := c.resumes[msg.SessionID]
		switch msg.Type {
		case protocol.TypeAttached:
			if tracked {
				if state.haveNext && msg.Seq > state.next && !msg.Snapshot {
					c.stateMu.Unlock()
					return protocol.Frame{}, false, fmt.Errorf(
						"transport: session %s resumed at sequence %d, want %d",
						msg.SessionID, msg.Seq, state.next,
					)
				}
				if !state.haveNext || msg.Seq > state.next {
					state.next = msg.Seq
					state.haveNext = true
				}
				c.resumes[msg.SessionID] = state
			}
		case protocol.TypeDetach, protocol.TypeExit:
			delete(c.resumes, msg.SessionID)
		}
		c.stateMu.Unlock()
		return frame, false, nil
	case protocol.KindData:
		key := frame.Session.String()
		c.stateMu.Lock()
		state, tracked := c.resumes[key]
		if !tracked {
			c.stateMu.Unlock()
			return frame, false, nil
		}
		if !state.haveNext {
			state.next = frame.Seq
			state.haveNext = true
		}
		if frame.Seq > state.next {
			c.stateMu.Unlock()
			return protocol.Frame{}, false, fmt.Errorf(
				"transport: session %s output gap: got sequence %d, want %d",
				key, frame.Seq, state.next,
			)
		}
		end := frame.Seq + uint64(len(frame.Payload))
		if end < frame.Seq {
			c.stateMu.Unlock()
			return protocol.Frame{}, false, fmt.Errorf("transport: session %s output sequence overflow", key)
		}
		if end <= state.next {
			c.stateMu.Unlock()
			return protocol.Frame{}, true, nil
		}
		if frame.Seq < state.next {
			overlap := state.next - frame.Seq
			frame.Payload = frame.Payload[overlap:]
			frame.Seq = state.next
		}
		state.next = frame.Seq + uint64(len(frame.Payload))
		c.resumes[key] = state
		c.stateMu.Unlock()
		return frame, false, nil
	default:
		return frame, false, nil
	}
}

func (c *reconnectingConn) sendResumes(conn *socketConn) error {
	frames, err := c.resumeFrames()
	if err != nil {
		return err
	}
	for _, frame := range frames {
		if err := conn.WriteFrame(frame); err != nil {
			return fmt.Errorf("transport: resume sessions: %w", err)
		}
	}
	return nil
}

func (c *reconnectingConn) resumeFrames() ([]protocol.Frame, error) {
	c.stateMu.Lock()
	keys := make([]string, 0, len(c.resumes))
	for key := range c.resumes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	states := make([]resumeState, len(keys))
	for i, key := range keys {
		states[i] = c.resumes[key]
	}
	c.stateMu.Unlock()

	frames := make([]protocol.Frame, 0, len(states))
	for _, state := range states {
		msg := state.attach
		if state.haveNext {
			next := state.next
			msg.LastSeq = &next
			msg.Tail = 0
		}
		payload, err := msg.Encode()
		if err != nil {
			return nil, fmt.Errorf("transport: encode resume for session %s: %w", msg.SessionID, err)
		}
		frames = append(frames, protocol.Frame{Kind: protocol.KindControl, Payload: payload})
	}
	return frames, nil
}

func backoffDelay(attempt int, opts Backoff, sample float64) time.Duration {
	delay := opts.Initial
	for range attempt {
		if delay >= opts.Max/2 {
			delay = opts.Max
			break
		}
		delay *= 2
	}
	if opts.Jitter == 0 {
		return delay
	}
	factor := 1 - opts.Jitter + 2*opts.Jitter*sample
	delay = time.Duration(float64(delay) * factor)
	if delay > opts.Max {
		return opts.Max
	}
	return delay
}

func cloneHeader(header http.Header) http.Header {
	if header == nil {
		return nil
	}
	return header.Clone()
}

func retryableReadError(err error) bool {
	return !errors.Is(err, ErrInvalidFrame) &&
		!errors.Is(err, ErrInboundQueueFull) &&
		!errors.Is(err, websocket.ErrMessageTooBig)
}

var _ Conn = (*reconnectingConn)(nil)
var _ Conn = (*socketConn)(nil)
