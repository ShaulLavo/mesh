package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

const (
	relayInputQueueFrameLimit  = 256
	relayInputQueueByteLimit   = 8 << 20
	relayOutputQueueFrameLimit = 256
	relayOutputControlReserve  = 8
	relayOutputQueueByteLimit  = 8 << 20
)

var (
	errRelayClosed          = errors.New("daemon: relay closed")
	errRelayGenerationStale = errors.New("daemon: stale relay generation")
	errRelayInputQueueFull  = errors.New("daemon: relay input queue full")
	errRelayOutputQueueFull = errors.New("daemon: relay output queue full")
)

// clientRelay owns the worker connections attached through one multiplexed
// client. The caller remains the sole reader of client frames and hands each
// one to HandleFrame.
type clientRelay struct {
	client  transport.Conn
	workers WorkerConnector

	lifetime context.Context
	cancel   context.CancelFunc

	// sendMu orders generation publication with worker output. Once a new lane
	// is current, an older lane can no longer cross this gate into the client.
	sendMu  sync.Mutex
	mu      sync.Mutex
	closed  bool
	next    uint64
	lanes   map[protocol.SessionID]*relayLane
	all     map[*relayLane]struct{}
	pending map[*relayCandidate]struct{}
	output  chan protocol.Frame

	outputMu     sync.Mutex
	outputFrames int
	outputBytes  int

	ops sync.WaitGroup
	wg  sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
}

type relayCandidate struct {
	conn      transport.Conn
	closeOnce sync.Once
	closeErr  error
}

type relayLane struct {
	relay      *clientRelay
	id         protocol.SessionID
	generation uint64
	worker     transport.Conn

	queue chan relayQueuedFrame
	done  chan struct{}

	queueMu      sync.Mutex
	queuedFrames int
	queuedBytes  int
	draining     bool
	closed       bool

	closeOnce sync.Once
	closeErr  error
}

type relayQueuedFrame struct {
	frame      protocol.Frame
	closeAfter bool
}

func (candidate *relayCandidate) close() error {
	candidate.closeOnce.Do(func() {
		candidate.closeErr = candidate.conn.Close()
	})
	return candidate.closeErr
}

func newClientRelay(client transport.Conn, workers WorkerConnector) *clientRelay {
	lifetime, cancel := context.WithCancel(context.Background())
	relay := &clientRelay{
		client:   client,
		workers:  workers,
		lifetime: lifetime,
		cancel:   cancel,
		lanes:    make(map[protocol.SessionID]*relayLane),
		all:      make(map[*relayLane]struct{}),
		pending:  make(map[*relayCandidate]struct{}),
		output:   make(chan protocol.Frame, relayOutputQueueFrameLimit+relayOutputControlReserve),
	}
	relay.wg.Add(1)
	go relay.writeClientLoop()
	return relay
}

// HandleFrame routes one client frame. False means the frame belongs to the
// daemon's lifecycle dispatcher rather than an attached worker.
func (r *clientRelay) HandleFrame(ctx context.Context, frame protocol.Frame) (bool, error) {
	if !r.beginOperation() {
		return true, errRelayClosed
	}
	defer r.ops.Done()

	switch frame.Kind {
	case protocol.KindControl:
		message, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return true, fmt.Errorf("daemon: decode client control: %w", err)
		}
		switch message.Type {
		case protocol.TypeAttach:
			id, err := relayControlSessionID(message)
			if err != nil {
				return true, err
			}
			return true, r.attach(ctx, id, frame)
		case protocol.TypeResize:
			id, err := relayControlSessionID(message)
			if err != nil {
				return true, err
			}
			return true, r.enqueue(id, frame, false)
		case protocol.TypeDetach:
			id, err := relayControlSessionID(message)
			if err != nil {
				return true, err
			}
			lane, err := r.currentLane(id)
			if err != nil {
				return true, err
			}
			if err := lane.enqueue(frame, true); err != nil {
				return true, err
			}
			// Stop accepting or forwarding for this session immediately, while
			// its writer preserves FIFO order through the detach itself.
			r.unpublish(lane)
			return true, nil
		case protocol.TypeAttached, protocol.TypeExit, protocol.TypeError:
			return true, fmt.Errorf("daemon: client sent worker response %q", message.Type)
		default:
			// Signals, kills, and daemon-level requests use one-shot lifecycle
			// paths. They must not acquire or disturb attachment ownership.
			return false, nil
		}

	case protocol.KindInput:
		if frame.Session == (protocol.SessionID{}) {
			return true, errors.New("daemon: input frame without session")
		}
		return true, r.enqueue(frame.Session, frame, false)

	case protocol.KindData, protocol.KindSnapshot:
		return true, fmt.Errorf("daemon: client sent worker output kind %d", frame.Kind)

	default:
		return true, fmt.Errorf("daemon: unsupported client frame kind %d", frame.Kind)
	}
}

func (r *clientRelay) beginOperation() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.ops.Add(1)
	return true
}

func (r *clientRelay) attach(ctx context.Context, id protocol.SessionID, frame protocol.Frame) error {
	dialCtx, cancel := context.WithCancel(ctx)
	stopLifetime := context.AfterFunc(r.lifetime, cancel)
	defer func() {
		stopLifetime()
		cancel()
	}()

	worker, err := r.workers.ConnectWorker(dialCtx, id)
	if err != nil {
		return relayAttachOperationError(dialCtx, id, "connect worker", err)
	}
	if worker == nil {
		return fmt.Errorf("daemon: connect session %s worker: nil connection", id.String())
	}
	candidate := &relayCandidate{conn: worker}
	if !r.registerCandidate(candidate) {
		_ = candidate.close()
		return errRelayClosed
	}
	published := false
	defer func() {
		if !published {
			r.removeCandidate(candidate)
			_ = candidate.close()
		}
	}()

	// Once dialing has returned, cancellation must close the candidate itself:
	// Conn operations do not take contexts, but Close is required to unblock
	// both reads and writes.
	stopCandidate := context.AfterFunc(dialCtx, func() { _ = candidate.close() })
	defer stopCandidate()

	if err := worker.WriteFrame(frame); err != nil {
		return relayAttachOperationError(dialCtx, id, "write attach", err)
	}

	first, err := worker.ReadFrame()
	if err != nil {
		return relayAttachOperationError(dialCtx, id, "read attach response", err)
	}
	response, err := validateWorkerAttachResponse(id, first)
	if err != nil {
		return err
	}
	if !stopCandidate() {
		_ = candidate.close()
		return relayAttachOperationError(dialCtx, id, "complete attach", transport.ErrClosed)
	}

	if response.Type == protocol.TypeError {
		if err := r.forwardCandidate(first); err != nil {
			return err
		}
		return nil
	}

	old, err := r.publishCandidate(candidate, id, first)
	if err != nil {
		return err
	}
	published = true
	if old != nil {
		_ = old.close()
	}
	return nil
}

func (r *clientRelay) registerCandidate(candidate *relayCandidate) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.pending[candidate] = struct{}{}
	return true
}

func (r *clientRelay) removeCandidate(candidate *relayCandidate) {
	r.mu.Lock()
	delete(r.pending, candidate)
	r.mu.Unlock()
}

func (r *clientRelay) publishCandidate(candidate *relayCandidate, id protocol.SessionID, attached protocol.Frame) (*relayLane, error) {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()

	r.mu.Lock()
	if r.closed {
		delete(r.pending, candidate)
		r.mu.Unlock()
		return nil, errRelayClosed
	}
	if _, ok := r.pending[candidate]; !ok {
		r.mu.Unlock()
		return nil, errRelayClosed
	}
	r.mu.Unlock()

	// The first response belongs to the candidate and enters the same FIFO as
	// every output frame. sendMu orders it after accepted incumbent output and
	// before the new generation can enqueue anything.
	if err := r.enqueueOutput(attached); err != nil {
		go r.Close()
		return nil, fmt.Errorf("daemon: queue session %s attach response: %w", id.String(), err)
	}

	r.mu.Lock()
	if r.closed {
		delete(r.pending, candidate)
		r.mu.Unlock()
		return nil, errRelayClosed
	}
	if _, ok := r.pending[candidate]; !ok {
		r.mu.Unlock()
		return nil, errRelayClosed
	}
	delete(r.pending, candidate)
	r.next++
	lane := &relayLane{
		relay:      r,
		id:         id,
		generation: r.next,
		worker:     candidate.conn,
		queue:      make(chan relayQueuedFrame, relayInputQueueFrameLimit),
		done:       make(chan struct{}),
	}
	old := r.lanes[id]
	r.lanes[id] = lane
	r.all[lane] = struct{}{}
	r.wg.Add(2)
	r.mu.Unlock()

	// Start while publication still holds sendMu. A fast worker can read, but
	// cannot emit output until the new generation is fully installed.
	go lane.readLoop()
	go lane.writeLoop()
	return old, nil
}

func (r *clientRelay) forwardCandidate(frame protocol.Frame) error {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()

	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return errRelayClosed
	}
	if err := r.enqueueOutput(frame); err != nil {
		go r.Close()
		return fmt.Errorf("daemon: queue rejected attach response: %w", err)
	}
	return nil
}

func (r *clientRelay) enqueue(id protocol.SessionID, frame protocol.Frame, closeAfter bool) error {
	lane, err := r.currentLane(id)
	if err != nil {
		return err
	}
	return lane.enqueue(frame, closeAfter)
}

func (r *clientRelay) currentLane(id protocol.SessionID) (*relayLane, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errRelayClosed
	}
	lane := r.lanes[id]
	if lane == nil {
		return nil, fmt.Errorf("daemon: session %s is not attached", id.String())
	}
	return lane, nil
}

func (l *relayLane) enqueue(frame protocol.Frame, closeAfter bool) error {
	queued := relayQueuedFrame{frame: cloneFrame(frame), closeAfter: closeAfter}
	bytes := len(queued.frame.Payload)

	l.queueMu.Lock()
	defer l.queueMu.Unlock()
	if l.closed || l.draining {
		return fmt.Errorf("daemon: session %s attachment closed", l.id.String())
	}
	if l.queuedFrames >= relayInputQueueFrameLimit || bytes > relayInputQueueByteLimit-l.queuedBytes {
		return fmt.Errorf("%w for session %s", errRelayInputQueueFull, l.id.String())
	}
	select {
	case l.queue <- queued:
		l.queuedFrames++
		l.queuedBytes += bytes
		l.draining = closeAfter
		return nil
	default:
		return fmt.Errorf("%w for session %s", errRelayInputQueueFull, l.id.String())
	}
}

func (l *relayLane) release(frame relayQueuedFrame) {
	l.queueMu.Lock()
	l.queuedFrames--
	l.queuedBytes -= len(frame.frame.Payload)
	l.queueMu.Unlock()
}

func (l *relayLane) writeLoop() {
	defer l.relay.wg.Done()
	for {
		select {
		case <-l.done:
			return
		default:
		}
		select {
		case queued := <-l.queue:
			err := l.worker.WriteFrame(queued.frame)
			l.release(queued)
			if err != nil {
				if l.relay.isClosed() {
					return
				}
				l.relay.retire(l)
				return
			}
			if queued.closeAfter {
				l.relay.retire(l)
				return
			}
		case <-l.done:
			return
		}
	}
}

func (l *relayLane) readLoop() {
	defer l.relay.wg.Done()
	for {
		frame, err := l.worker.ReadFrame()
		if err != nil {
			if l.relay.isClosed() {
				return
			}
			l.relay.retire(l)
			return
		}
		if err := validateWorkerFrame(l.id, frame); err != nil {
			if l.relay.isClosed() {
				return
			}
			l.relay.retire(l)
			return
		}
		if err := l.relay.forward(l, frame); err != nil {
			if errors.Is(err, errRelayClosed) || l.relay.isClosed() {
				return
			}
			if errors.Is(err, errRelayGenerationStale) && l.isDraining() {
				return
			}
			l.relay.retire(l)
			if !errors.Is(err, errRelayGenerationStale) && !errors.Is(err, errRelayClosed) {
				go l.relay.Close() // Close cannot wait on the reader that invoked it.
			}
			return
		}
	}
}

func (r *clientRelay) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func (l *relayLane) isDraining() bool {
	l.queueMu.Lock()
	defer l.queueMu.Unlock()
	return l.draining
}

func (r *clientRelay) forward(lane *relayLane, frame protocol.Frame) error {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()

	r.mu.Lock()
	current := !r.closed && r.lanes[lane.id] == lane
	r.mu.Unlock()
	if !current {
		return errRelayGenerationStale
	}
	if err := r.enqueueOutput(frame); err != nil {
		return fmt.Errorf("daemon: queue session %s output: %w", lane.id.String(), err)
	}
	return nil
}

func (r *clientRelay) enqueueOutput(frame protocol.Frame) error {
	queued := cloneFrame(frame)
	payload := queued.Kind == protocol.KindData || queued.Kind == protocol.KindSnapshot
	bytes := len(queued.Payload)

	r.outputMu.Lock()
	defer r.outputMu.Unlock()
	select {
	case <-r.lifetime.Done():
		return errRelayClosed
	default:
	}
	if payload && (r.outputFrames >= relayOutputQueueFrameLimit || bytes > relayOutputQueueByteLimit-r.outputBytes) {
		return errRelayOutputQueueFull
	}
	select {
	case r.output <- queued:
		if payload {
			r.outputFrames++
			r.outputBytes += bytes
		}
		return nil
	default:
		return errRelayOutputQueueFull
	}
}

func (r *clientRelay) releaseOutput(frame protocol.Frame) {
	if frame.Kind != protocol.KindData && frame.Kind != protocol.KindSnapshot {
		return
	}
	r.outputMu.Lock()
	r.outputFrames--
	r.outputBytes -= len(frame.Payload)
	r.outputMu.Unlock()
}

func (r *clientRelay) writeClientLoop() {
	defer r.wg.Done()
	for {
		select {
		case frame := <-r.output:
			err := r.client.WriteFrame(frame)
			r.releaseOutput(frame)
			if err != nil {
				go r.Close()
				return
			}
		case <-r.lifetime.Done():
			return
		}
	}
}

func (r *clientRelay) unpublish(lane *relayLane) {
	r.sendMu.Lock()
	r.mu.Lock()
	if r.lanes[lane.id] == lane {
		delete(r.lanes, lane.id)
	}
	r.mu.Unlock()
	r.sendMu.Unlock()
}

func (r *clientRelay) retire(lane *relayLane) {
	r.unpublish(lane)
	_ = lane.close()
	r.mu.Lock()
	delete(r.all, lane)
	r.mu.Unlock()
}

func (l *relayLane) close() error {
	l.closeOnce.Do(func() {
		l.queueMu.Lock()
		l.closed = true
		close(l.done)
		l.queueMu.Unlock()
		l.closeErr = l.worker.Close()
	})
	return l.closeErr
}

// Close tears down relay sockets only. Closing a worker connection never sends
// detach, signal, or kill, so the worker-owned process survives daemon death.
func (r *clientRelay) Close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.cancel()
		r.mu.Unlock()

		// Conn.Close is the cancellation signal for a blocked client write. It
		// must happen before waiting on sendMu or any worker goroutine.
		clientErr := r.client.Close()

		r.sendMu.Lock()
		r.mu.Lock()
		lanes := make([]*relayLane, 0, len(r.all))
		for lane := range r.all {
			lanes = append(lanes, lane)
		}
		candidates := make([]*relayCandidate, 0, len(r.pending))
		for candidate := range r.pending {
			candidates = append(candidates, candidate)
		}
		clear(r.lanes)
		clear(r.all)
		clear(r.pending)
		r.mu.Unlock()
		r.sendMu.Unlock()

		var closeErr error
		for _, candidate := range candidates {
			closeErr = errors.Join(closeErr, candidate.close())
		}
		for _, lane := range lanes {
			closeErr = errors.Join(closeErr, lane.close())
		}
		r.ops.Wait()
		r.wg.Wait()
		r.closeErr = errors.Join(clientErr, closeErr)
	})
	return r.closeErr
}

func relayControlSessionID(message protocol.Control) (protocol.SessionID, error) {
	id, err := protocol.NewSessionID(message.SessionID)
	if err != nil {
		return protocol.SessionID{}, fmt.Errorf("daemon: %s session ID: %w", message.Type, err)
	}
	if id.String() != message.SessionID {
		return protocol.SessionID{}, fmt.Errorf("daemon: %s has non-canonical session ID %q", message.Type, message.SessionID)
	}
	return id, nil
}

func relayAttachOperationError(ctx context.Context, id protocol.SessionID, operation string, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		err = contextErr
	}
	return fmt.Errorf("daemon: session %s %s: %w", id.String(), operation, err)
}

func validateWorkerAttachResponse(id protocol.SessionID, frame protocol.Frame) (protocol.Control, error) {
	if frame.Kind != protocol.KindControl {
		return protocol.Control{}, fmt.Errorf("daemon: session %s worker attach response has kind %d", id.String(), frame.Kind)
	}
	message, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: session %s worker attach response: %w", id.String(), err)
	}
	if message.SessionID != id.String() {
		return protocol.Control{}, fmt.Errorf("daemon: session %s worker attach response names %q", id.String(), message.SessionID)
	}
	switch message.Type {
	case protocol.TypeAttached, protocol.TypeError:
		return message, nil
	default:
		return protocol.Control{}, fmt.Errorf("daemon: session %s worker attach response has type %q", id.String(), message.Type)
	}
}

func validateWorkerFrame(id protocol.SessionID, frame protocol.Frame) error {
	switch frame.Kind {
	case protocol.KindControl:
		message, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return fmt.Errorf("daemon: session %s worker control: %w", id.String(), err)
		}
		if message.SessionID != id.String() {
			return fmt.Errorf("daemon: session %s worker sent control for %q", id.String(), message.SessionID)
		}
		switch message.Type {
		case protocol.TypeAttached, protocol.TypeDetach, protocol.TypeExit, protocol.TypeError:
			return nil
		default:
			return fmt.Errorf("daemon: session %s worker sent invalid control %q", id.String(), message.Type)
		}
	case protocol.KindData, protocol.KindSnapshot:
		if frame.Session != id {
			return fmt.Errorf("daemon: session %s worker sent frame for %s", id.String(), frame.Session.String())
		}
		return nil
	default:
		return fmt.Errorf("daemon: session %s worker sent invalid frame kind %d", id.String(), frame.Kind)
	}
}

func cloneFrame(frame protocol.Frame) protocol.Frame {
	frame.Payload = bytes.Clone(frame.Payload)
	return frame
}
