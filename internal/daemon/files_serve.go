package daemon

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

// frameLookahead lets a long request notice that its caller left. The client
// loop is the only reader of its connection, so while a request runs nothing
// would see a closed socket. watch starts the next read early; if it fails the
// request's context ends, and if it returns a frame the loop receives that
// frame next, in order.
type frameLookahead struct {
	conn    transport.Conn
	pending chan lookaheadRead
	armed   bool
}

type lookaheadRead struct {
	frame protocol.Frame
	err   error
}

func newFrameLookahead(conn transport.Conn) *frameLookahead { return &frameLookahead{conn: conn} }

// read returns the next frame from the connection, taking the one watch
// already started if there is one.
func (l *frameLookahead) read() (protocol.Frame, error) {
	if !l.armed {
		return l.conn.ReadFrame() //nolint:wrapcheck // the client loop classifies the raw transport error
	}
	l.armed = false
	result := <-l.pending
	return result.frame, result.err
}

// watch returns a context that ends when the connection fails.
func (l *frameLookahead) watch(ctx context.Context) (context.Context, context.CancelFunc) {
	watched, cancel := context.WithCancel(ctx)
	l.pending = make(chan lookaheadRead, 1)
	l.armed = true
	pending := l.pending
	go func() {
		frame, err := l.conn.ReadFrame()
		pending <- lookaheadRead{frame: frame, err: err}
		if err != nil {
			cancel()
		}
	}()
	return watched, cancel
}

// Exclusive watch states: the request runs until either its reply is
// committed or another frame interrupts it, never both.
const (
	watchRunning int32 = iota
	watchCommitted
	watchInterrupted
)

// lookaheadFrameClaimedHook runs after watchAny claims a read and before it
// publishes it to pending; tests install it to order a frame against a
// reply. Atomic, since a watcher from another connection may be finishing
// while a test installs it.
var lookaheadFrameClaimedHook atomic.Pointer[func(pending chan lookaheadRead, err error)]

// lookaheadInterruptClaimedHook runs right after the watcher claims the
// interruption and before it records why; tests pause there.
var lookaheadInterruptClaimedHook atomic.Pointer[func(error)]

func lookaheadInterruptClaimed(err error) {
	if hook := lookaheadInterruptClaimedHook.Load(); hook != nil {
		(*hook)(err)
	}
}

func lookaheadFrameClaimed(pending chan lookaheadRead, err error) {
	if hook := lookaheadFrameClaimedHook.Load(); hook != nil {
		(*hook)(pending, err)
	}
}

// watchAny is watch for a request that must be its connection's only one in
// flight: a frame that arrives before the reply is committed interrupts the
// request and ends ctx, as a failure does. The context's cause says which:
// errRunEnded for a frame, or the read's own error, so the client loop can
// tell a broken stream from a caller that left. commit reports whether the
// reply may still be sent; once it returns true, the next frame is simply
// the loop's next request.
func (l *frameLookahead) watchAny(ctx context.Context) (watched context.Context, cancel context.CancelFunc, commit func() bool) {
	watched, cancelCause := context.WithCancelCause(ctx)
	l.pending = make(chan lookaheadRead, 1)
	l.armed = true
	pending := l.pending
	var state atomic.Int32
	go func() {
		frame, err := l.conn.ReadFrame()
		// Claim the interruption before the read becomes visible, so a reply
		// can never be committed with this frame already pending.
		interrupted := state.CompareAndSwap(watchRunning, watchInterrupted)
		lookaheadInterruptClaimed(err)
		if err != nil {
			cancelCause(err)
		} else if interrupted {
			cancelCause(errRunEnded)
		}
		lookaheadFrameClaimed(pending, err)
		pending <- lookaheadRead{frame: frame, err: err}
	}()
	commit = func() bool { return state.CompareAndSwap(watchRunning, watchCommitted) }
	return watched, func() { cancelCause(nil) }, commit
}

// replyFiles runs one file request and queues its reply with the grant check
// the relay applies when the frame leaves the queue, so a device revoked while
// the request ran, or while its reply waited, receives nothing.
func (s *clientServer) replyFiles(ctx context.Context, relay *clientRelay, lookahead *frameLookahead, request protocol.Control) error {
	requestCtx, cancel := lookahead.watch(ctx)
	reply, _, err := s.files.HandleControl(requestCtx, request)
	cancel()
	if err != nil {
		reply = clientRequestError(request, err)
	}
	frames, err := protocol.ControlFrames(reply)
	if err == nil && len(frames) != 1 {
		err = fmt.Errorf("daemon: %s reply needs %d frames", request.Type, len(frames))
	}
	if err != nil {
		return writeClientRequestError(relay, request, fmt.Errorf("daemon: %s reply does not fit one frame: %w", request.Type, err))
	}
	return relay.enqueueAdmittedResponse(frames[0], func() error { return workerExecutionAdmission(ctx) })
}
