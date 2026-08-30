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
	attach          protocol.Control
	next            uint64
	haveNext        bool
	pendingSnapshot *snapshotCheckpoint
}

type snapshotCheckpoint struct {
	generation uint64
	next       uint64
}

type linkConn interface {
	Conn
	fail(error)
	stopped() bool
}

type connectionRef struct {
	link       linkConn
	generation uint64
}

type linkDialer func(context.Context, string, websocket.DialOptions, KeepAlive) (linkConn, error)

type reconnectingConn struct {
	url       string
	dialOpts  websocket.DialOptions
	keepAlive KeepAlive
	backoff   Backoff
	ctx       context.Context
	cancel    context.CancelFunc
	dial      linkDialer

	readMu  sync.Mutex
	writeMu sync.Mutex
	// reconnectMu linearizes socket replacement with every event that can
	// change resume state. It is never held across a blocking ReadFrame.
	reconnectMu sync.Mutex
	stateMu     sync.Mutex
	current     connectionRef
	connecting  linkConn
	generation  uint64
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
		url:        url,
		dialOpts:   dialOpts,
		keepAlive:  normalized.keepAlive,
		backoff:    normalized.backoff,
		ctx:        lifetime,
		cancel:     cancel,
		dial:       dialLink,
		current:    connectionRef{link: initial, generation: 1},
		generation: 1,
		resumes:    make(map[string]resumeState),
	}, nil
}

// DialOnce opens one WebSocket link and never reconnects it. Use this for
// authenticated request sequences whose trust pin applies to one exact peer
// generation.
func DialOnce(ctx context.Context, url string, opts DialOptions) (Conn, error) {
	normalized, err := normalizeDialOptions(opts)
	if err != nil {
		return nil, err
	}
	dialOpts := websocket.DialOptions{
		HTTPClient:      opts.HTTPClient,
		HTTPHeader:      cloneHeader(opts.HTTPHeader),
		CompressionMode: websocket.CompressionDisabled,
	}
	return dialSocket(ctx, url, dialOpts, normalized.keepAlive)
}

// dialLink adapts dialSocket to the linkDialer signature. The nil check is
// load-bearing: returning dialSocket's (*socketConn)(nil) directly would box a
// typed nil into a non-nil linkConn, and connectionLocked's `link != nil` guard
// would then call Close on it.
func dialLink(ctx context.Context, url string, opts websocket.DialOptions, keepAlive KeepAlive) (linkConn, error) {
	link, err := dialSocket(ctx, url, opts, keepAlive)
	if err != nil {
		return nil, err
	}
	return link, nil
}

func dialSocket(ctx context.Context, url string, opts websocket.DialOptions, keepAlive KeepAlive) (*socketConn, error) {
	ws, response, err := websocket.Dial(ctx, url, &opts) //nolint:bodyclose // websocket.Dial owns and closes its HTTP response body
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
		ref, err := c.connection(connectionRef{})
		if err != nil {
			return protocol.Frame{}, err
		}
		frame, err := ref.link.ReadFrame()
		if err != nil {
			if !retryableReadError(err) {
				c.retire(ref)
				return protocol.Frame{}, err
			}
			if _, reconnectErr := c.connection(ref); reconnectErr != nil {
				return protocol.Frame{}, reconnectErr
			}
			continue
		}
		frame, drop, err := c.observeFrame(ref, frame)
		if err != nil {
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
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()

	ref, err := c.connectionLocked(connectionRef{})
	if err != nil {
		return err
	}
	if ref.link.stopped() {
		c.retireLocked(ref)
		ref, err = c.connectionLocked(ref)
		if err != nil {
			return err
		}
	}
	if err := ref.link.WriteFrame(frame); err != nil {
		c.retireLocked(ref)
		return err
	}

	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.closed || !c.isCurrentLocked(ref) {
		return ErrClosed
	}
	c.observeWriteLocked(frame)
	return nil
}

func (c *reconnectingConn) Close() error {
	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return nil
	}
	c.closed = true
	ref := c.current
	connecting := c.connecting
	c.current = connectionRef{}
	c.connecting = nil
	c.stateMu.Unlock()
	c.cancel()

	var err error
	if ref.link != nil {
		err = ref.link.Close()
	}
	if connecting != nil {
		err = errors.Join(err, connecting.Close())
	}
	// A concurrent reconnect observes the cancelled context and exits before
	// Close returns.
	c.reconnectMu.Lock()
	c.reconnectMu.Unlock() //nolint:staticcheck // lock and unlock form a completion barrier for reconnect
	c.writeMu.Lock()
	c.writeMu.Unlock() //nolint:staticcheck // lock and unlock form a completion barrier for writes
	return err
}

func (c *reconnectingConn) connection(failed connectionRef) (connectionRef, error) {
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()
	return c.connectionLocked(failed)
}

func (c *reconnectingConn) connectionLocked(failed connectionRef) (connectionRef, error) {
	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return connectionRef{}, ErrClosed
	}
	if c.current.link != nil {
		if failed.generation != 0 && c.isCurrentLocked(failed) {
			c.current = connectionRef{}
		} else {
			ref := c.current
			c.stateMu.Unlock()
			return ref, nil
		}
	}
	c.stateMu.Unlock()

	for attempt := 0; ; attempt++ {
		link, err := c.dial(c.ctx, c.url, c.dialOpts, c.keepAlive)
		if err == nil {
			c.stateMu.Lock()
			if c.closed {
				c.stateMu.Unlock()
				_ = link.Close()
				return connectionRef{}, ErrClosed
			}
			c.connecting = link
			c.stateMu.Unlock()

			err = c.sendResumes(link)
			c.stateMu.Lock()
			c.connecting = nil
			c.stateMu.Unlock()
		}
		if err == nil {
			c.stateMu.Lock()
			if c.closed {
				c.stateMu.Unlock()
				_ = link.Close()
				return connectionRef{}, ErrClosed
			}
			c.generation++
			ref := connectionRef{link: link, generation: c.generation}
			c.current = ref
			c.stateMu.Unlock()
			return ref, nil
		}
		if link != nil {
			_ = link.Close()
		}

		delay := backoffDelay(attempt, c.backoff, rand.Float64()) //nolint:gosec // retry jitter is not a security decision
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
			return connectionRef{}, ErrClosed
		}
	}
}

func (c *reconnectingConn) retire(ref connectionRef) {
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()
	c.retireLocked(ref)
}

func (c *reconnectingConn) retireLocked(ref connectionRef) {
	c.stateMu.Lock()
	if c.isCurrentLocked(ref) {
		c.current = connectionRef{}
	}
	c.stateMu.Unlock()
}

func (c *reconnectingConn) isCurrentLocked(ref connectionRef) bool {
	return ref.link != nil && c.current.link != nil && ref.generation != 0 &&
		c.current.generation == ref.generation
}

func (c *reconnectingConn) observeWriteLocked(frame protocol.Frame) {
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
		c.resumes[msg.SessionID] = state
	case protocol.TypeDetach:
		delete(c.resumes, msg.SessionID)
	}
}

func (c *reconnectingConn) observeFrame(ref connectionRef, frame protocol.Frame) (protocol.Frame, bool, error) {
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()

	c.stateMu.Lock()
	if !c.isCurrentLocked(ref) {
		c.stateMu.Unlock()
		return protocol.Frame{}, true, nil
	}
	frame, drop, err := c.observeFrameLocked(ref.generation, frame)
	if err != nil {
		c.current = connectionRef{}
	}
	c.stateMu.Unlock()
	if err != nil {
		ref.link.fail(err)
	}
	return frame, drop, err
}

func (c *reconnectingConn) observeFrameLocked(generation uint64, frame protocol.Frame) (protocol.Frame, bool, error) {
	switch frame.Kind {
	case protocol.KindControl:
		msg, err := protocol.DecodeControl(frame.Payload)
		if err != nil || msg.SessionID == "" {
			return frame, false, nil
		}
		state, tracked := c.resumes[msg.SessionID]
		switch msg.Type {
		case protocol.TypeAttached:
			if tracked {
				if msg.Snapshot {
					if state.haveNext && msg.Seq < state.next {
						return protocol.Frame{}, false, fmt.Errorf(
							"transport: session %s snapshot at sequence %d, behind %d",
							msg.SessionID, msg.Seq, state.next,
						)
					}
					state.pendingSnapshot = &snapshotCheckpoint{
						generation: generation,
						next:       msg.Seq,
					}
				} else if state.haveNext && msg.Seq > state.next {
					return protocol.Frame{}, false, fmt.Errorf(
						"transport: session %s resumed at sequence %d, want %d",
						msg.SessionID, msg.Seq, state.next,
					)
				} else if !state.haveNext {
					state.next = msg.Seq
					state.haveNext = true
				}
				if !msg.Snapshot {
					state.pendingSnapshot = nil
				}
				c.resumes[msg.SessionID] = state
			}
		case protocol.TypeDetach, protocol.TypeExit:
			delete(c.resumes, msg.SessionID)
		}
		return frame, false, nil
	case protocol.KindSnapshot:
		key := frame.Session.String()
		state, tracked := c.resumes[key]
		if !tracked || state.pendingSnapshot == nil || state.pendingSnapshot.generation != generation {
			return frame, false, nil
		}
		state.next = state.pendingSnapshot.next
		state.haveNext = true
		state.pendingSnapshot = nil
		c.resumes[key] = state
		return frame, false, nil
	case protocol.KindData:
		key := frame.Session.String()
		state, tracked := c.resumes[key]
		if !tracked {
			return frame, false, nil
		}
		if state.pendingSnapshot != nil && state.pendingSnapshot.generation == generation {
			return protocol.Frame{}, false, fmt.Errorf(
				"transport: session %s received data before its announced snapshot",
				key,
			)
		}
		if !state.haveNext {
			state.next = frame.Seq
			state.haveNext = true
		}
		if frame.Seq > state.next {
			return protocol.Frame{}, false, fmt.Errorf(
				"transport: session %s output gap: got sequence %d, want %d",
				key, frame.Seq, state.next,
			)
		}
		end := frame.Seq + uint64(len(frame.Payload))
		if end < frame.Seq {
			return protocol.Frame{}, false, fmt.Errorf("transport: session %s output sequence overflow", key)
		}
		if end <= state.next {
			return protocol.Frame{}, true, nil
		}
		if frame.Seq < state.next {
			overlap := state.next - frame.Seq
			frame.Payload = frame.Payload[overlap:]
			frame.Seq = state.next
		}
		state.next = frame.Seq + uint64(len(frame.Payload))
		c.resumes[key] = state
		return frame, false, nil
	default:
		return frame, false, nil
	}
}

func (c *reconnectingConn) sendResumes(conn linkConn) error {
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

// retryableReadError distinguishes a peer that is speaking the protocol wrongly,
// where reconnecting would only repeat the failure, from a link that is merely
// gone. ErrInboundQueueFull is deliberately absent: it is local backpressure
// from a slow stdout, not a peer fault, and the resume offset is exact at that
// moment, so one redial resumes or snapshots cleanly. Reconnection is lazy and
// consumer-driven, so a consumer that is still stalled cannot spin on it.
func retryableReadError(err error) bool {
	return !errors.Is(err, ErrInvalidFrame) &&
		!errors.Is(err, websocket.ErrMessageTooBig)
}

var _ Conn = (*reconnectingConn)(nil)
var _ Conn = (*socketConn)(nil)
