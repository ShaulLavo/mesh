package mcphost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shaul/mesh/internal/mcpwire"
)

// Section 2.5 budgets. A call's queue always fits one maximum reply, so only
// a caller that stops reading loses its call.
const (
	callQueueBytes     = 8 << 20
	instanceQueueBytes = 32 << 20
	hostQueueBytes     = 128 << 20
	cancelWriteTimeout = 5 * time.Second
)

// ErrProtocolFault means an upstream broke framing or attribution after it
// was ready. The instance is faulted and every in-flight call fails.
var ErrProtocolFault = errors.New("mcp upstream: protocol fault")

// StdioUpstream multiplexes calls over one worker line attachment. Its reader
// never blocks on a caller: replies go to per-call queues bounded by bytes.
type StdioUpstream struct {
	conn   LineConn
	ids    *hostIDs
	budget *byteBudget
	// ctx bounds writes. A caller's cancellation never cancels a write that
	// is already in progress, which would leave the pipe misaligned.
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	calls    map[string]*stdioCall
	progress map[string]*stdioCall
	cancels  chan string
	done     chan struct{}
	err      error

	late    atomic.Uint64
	dropped atomic.Uint64
}

type queuedEvent struct {
	event mcpwire.Event
	size  int
}

type stdioCall struct {
	id       string
	token    string
	exchange *mcpwire.Exchange
	signal   chan struct{}

	queue  []queuedEvent
	bytes  int
	failed error
	// inHand marks queue[0] as being delivered to the caller right now.
	inHand bool
}

// newStdioUpstream starts the reader and the cancellation writer. The
// upstream lives until Close or a fault, not until parent's deadline, which
// may only bound the probe that follows.
func newStdioUpstream(parent context.Context, conn LineConn, router *Router) *StdioUpstream {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	u := &StdioUpstream{conn: conn, ids: router.ids, budget: &byteBudget{limit: instanceQueueBytes, parent: router.budget}, ctx: ctx, cancel: cancel, calls: make(map[string]*stdioCall), progress: make(map[string]*stdioCall), cancels: make(chan string, maxQueuedCancels), done: make(chan struct{})}
	go u.readLoop()
	go u.cancelLoop(ctx)
	return u
}

// maxQueuedCancels bounds cancellations waiting for the pipe. One more than
// that means the pipe is stuck, and the instance is faulted instead.
const maxQueuedCancels = 1024

// queueCancel asks the server to stop one request without blocking the
// caller or the reader.
func (u *StdioUpstream) queueCancel(id string) {
	select {
	case u.cancels <- id:
	default:
		go u.fault(fmt.Errorf("%w: cancellations are not draining", ErrProtocolFault))
	}
}

func (u *StdioUpstream) cancelLoop(ctx context.Context) {
	for {
		select {
		case id := <-u.cancels:
			u.sendCancel(ctx, id)
		case <-ctx.Done():
			return
		}
	}
}

// LateReplies counts attributable replies that arrived after their call
// ended or that were addressed to an earlier daemon run.
func (u *StdioUpstream) LateReplies() uint64 { return u.late.Load() }

func (u *StdioUpstream) Done() <-chan struct{} { return u.done }

func (u *StdioUpstream) Err() error {
	select {
	case <-u.done:
		return u.err
	default:
		return nil
	}
}

func (u *StdioUpstream) Close() error {
	u.fault(errUpstreamClosed)
	return nil
}

var errUpstreamClosed = errors.New("mcp upstream: connection closed")

func (u *StdioUpstream) fault(err error) {
	u.mu.Lock()
	select {
	case <-u.done:
		u.mu.Unlock()
		return
	default:
	}
	u.err = err
	close(u.done)
	calls := u.calls
	u.calls, u.progress = map[string]*stdioCall{}, map[string]*stdioCall{}
	for _, call := range calls {
		u.failLocked(call, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err))
	}
	u.mu.Unlock()
	u.cancel()
	_ = u.conn.Close()
}

func (u *StdioUpstream) readLoop() {
	for {
		line, err := u.conn.ReadLine()
		var oversized *ErrOversizedLine
		if errors.As(err, &oversized) {
			if err := u.routeOversized(oversized.ID); err != nil {
				u.fault(err)
				return
			}
			continue
		}
		if errors.Is(err, ErrLineFault) {
			u.fault(fmt.Errorf("%w: %w", ErrProtocolFault, err))
			return
		}
		if err != nil {
			// The server exited or the attachment closed; framing was intact.
			u.fault(fmt.Errorf("mcp upstream: %w", err))
			return
		}
		if err := u.route(line); err != nil {
			u.fault(err)
			return
		}
	}
}

// routeOversized fails only the correlated request when an oversized reply's
// delimiter and ID were recovered, keeping the server instance usable.
func (u *StdioUpstream) routeOversized(id mcpwire.ID) error {
	raw, _ := id.MarshalJSON()
	value, ok := decodeJSONString(raw)
	u.mu.Lock()
	call := u.calls[value]
	if call != nil && ok {
		u.endLocked(call, fmt.Errorf("reply exceeds %d bytes: %w", mcpwire.MaxMessage, mcpwire.ErrTooLarge), false)
		u.mu.Unlock()
		return nil
	}
	u.mu.Unlock()
	return u.unmatched(value, ok, "request ID")
}

func (u *StdioUpstream) route(line []byte) error {
	m, fault := mcpwire.Parse(line)
	if fault != nil {
		return fmt.Errorf("%w: unparseable stdout line: %s", ErrProtocolFault, fault.Message)
	}
	id, token := m.Correlation()
	switch {
	case m.Kind() == mcpwire.Request:
		return fmt.Errorf("%w: server sent request %q", ErrProtocolFault, m.Method())
	case token != nil:
		return u.routeProgress(line, token)
	case id.Valid():
		return u.routeID(line, id)
	case m.Kind() == mcpwire.Notification && m.Method() == "notifications/message":
		// Stdio logs carry no attribution. Dropping one loses no framing.
		u.dropped.Add(1)
		return nil
	default:
		return fmt.Errorf("%w: unattributable %s message", ErrProtocolFault, kindName(m))
	}
}

func kindName(m mcpwire.Message) string {
	if m.Method() != "" {
		return m.Method()
	}
	return "response"
}

func (u *StdioUpstream) routeProgress(line, token []byte) error {
	value, ok := decodeJSONString(token)
	u.mu.Lock()
	call := u.progress[value]
	u.mu.Unlock()
	if call == nil {
		return u.unmatched(value, ok, "progress token")
	}
	return u.deliver(call, line)
}

func (u *StdioUpstream) routeID(line []byte, id mcpwire.ID) error {
	raw, _ := id.MarshalJSON()
	value, ok := decodeJSONString(raw)
	u.mu.Lock()
	call := u.calls[value]
	u.mu.Unlock()
	if call == nil || !ok {
		return u.unmatched(value, ok, "request ID")
	}
	return u.deliver(call, line)
}

// unmatched keeps the pipe usable for replies that this host can attribute
// to a finished call or an earlier run, and faults it for anything else.
func (u *StdioUpstream) unmatched(value string, isString bool, what string) error {
	if isString && u.ids.owner(value) != foreignID {
		u.late.Add(1)
		return nil
	}
	return fmt.Errorf("%w: reply with unknown %s", ErrProtocolFault, what)
}

func (u *StdioUpstream) deliver(call *stdioCall, line []byte) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.calls[call.id] != call {
		u.late.Add(1)
		return nil
	}
	event, err := call.exchange.Accept(line)
	if err != nil {
		// Framing and attribution are intact, so only this call fails.
		u.endLocked(call, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err), true)
		return nil
	}
	if event.Terminal {
		u.forgetLocked(call)
	}
	u.enqueueLocked(call, event, len(line))
	return nil
}

func (u *StdioUpstream) enqueueLocked(call *stdioCall, event mcpwire.Event, size int) {
	// The event in the caller's hands is never replaced under it.
	if last := len(call.queue) - 1; last >= 0 && !(last == 0 && call.inHand) && coalescible(call.queue[last].event, event) {
		u.budget.release(call.queue[last].size)
		call.bytes -= call.queue[last].size
		call.queue = call.queue[:last]
	}
	if call.bytes+size > callQueueBytes || !u.budget.take(size) {
		u.dropQueuedLocked(call)
		u.endLocked(call, ErrSlowConsumer, !event.Terminal)
		return
	}
	call.queue = append(call.queue, queuedEvent{event, size})
	call.bytes += size
	notify(call.signal)
}

// coalescible replaces a progress update the caller has not read yet with a
// newer one for the same token.
func coalescible(queued, next mcpwire.Event) bool {
	return !queued.Terminal && !next.Terminal && queued.Message.Method() == progressMethod && next.Message.Method() == progressMethod
}

func notify(signal chan struct{}) {
	select {
	case signal <- struct{}{}:
	default:
	}
}

func (u *StdioUpstream) endLocked(call *stdioCall, err error, cancelUpstream bool) {
	active := u.calls[call.id] == call
	u.forgetLocked(call)
	u.failLocked(call, err)
	if active && cancelUpstream {
		u.queueCancel(call.id)
	}
}

func (u *StdioUpstream) failLocked(call *stdioCall, err error) {
	if call.failed == nil {
		call.failed = err
	}
	notify(call.signal)
}

func (u *StdioUpstream) forgetLocked(call *stdioCall) {
	if u.calls[call.id] == call {
		delete(u.calls, call.id)
	}
	if call.token != "" && u.progress[call.token] == call {
		delete(u.progress, call.token)
	}
}

func (u *StdioUpstream) sendCancel(ctx context.Context, id string) {
	raw, err := encodeJSON(map[string]any{jsonrpcKey: jsonrpcVersion, "method": "notifications/cancelled", paramsKey: map[string]any{"requestId": id}})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, cancelWriteTimeout)
	defer cancel()
	_ = u.conn.WriteLine(ctx, append(raw, '\n'))
}

// Call implements Upstream.
func (u *StdioUpstream) Call(ctx context.Context, request mcpwire.Message, sink func(mcpwire.Event) error) error {
	call, err := u.register(request)
	if err != nil {
		return err
	}
	line, err := stdioLine(request.Raw())
	if err != nil {
		u.abandon(call, false)
		return fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	// A caller leaving mid-write must not cut a line in half; the worker's
	// acknowledgment deadline bounds the write instead.
	if err := u.conn.WriteLine(context.WithoutCancel(ctx), line); err != nil {
		u.abandon(call, false)
		return fmt.Errorf("write %s request: %w", request.Method(), err)
	}
	return u.drain(ctx, call, sink)
}

func stdioLine(raw []byte) ([]byte, error) {
	var line bytes.Buffer
	line.Grow(len(raw) + 1)
	if err := json.Compact(&line, raw); err != nil {
		return nil, fmt.Errorf("compact request: %w", err)
	}
	line.WriteByte('\n')
	return line.Bytes(), nil
}

func (u *StdioUpstream) register(request mcpwire.Message) (*stdioCall, error) {
	raw, _ := request.ID().MarshalJSON()
	id, ok := decodeJSONString(raw)
	if !ok || u.ids.owner(id) != currentRunID {
		return nil, fmt.Errorf("%w: request ID was not assigned by this host", ErrNotSent)
	}
	token, _ := decodeJSONString(member(request.Raw(), paramsKey, metaKey, "progressToken"))
	call := &stdioCall{id: id, token: token, exchange: mcpwire.NewExchange(request, mcpwire.StdioReply), signal: make(chan struct{}, 1)}
	u.mu.Lock()
	defer u.mu.Unlock()
	select {
	case <-u.done:
		return nil, fmt.Errorf("%w: %w", ErrNotSent, u.err)
	default:
	}
	if u.calls[id] != nil {
		return nil, fmt.Errorf("%w: duplicate active request ID", ErrNotSent)
	}
	u.calls[id] = call
	if token != "" {
		u.progress[token] = call
	}
	return call, nil
}

// abandon releases a call. A call that may have reached the server gets an
// exact cancellation so the server stops working on it.
func (u *StdioUpstream) abandon(call *stdioCall, sent bool) {
	u.mu.Lock()
	active := u.calls[call.id] == call
	u.forgetLocked(call)
	for _, queued := range call.queue {
		u.budget.release(queued.size)
	}
	call.queue, call.bytes, call.inHand = nil, 0, false
	u.mu.Unlock()
	if active && sent {
		u.queueCancel(call.id)
	}
}

// drain delivers one event at a time. The event in the caller's hands stays
// charged to the call and the budgets until sink returns, so a stalled
// caller is counted for everything it holds.
func (u *StdioUpstream) drain(ctx context.Context, call *stdioCall, sink func(mcpwire.Event) error) error {
	for {
		queued, ok, failed := u.next(call)
		if ok {
			if err := u.deliverOne(ctx, call, queued, sink); err != nil || queued.event.Terminal {
				return err
			}
			continue
		}
		if failed != nil {
			u.abandon(call, false)
			return failed
		}
		select {
		case <-call.signal:
		case <-ctx.Done():
			u.abandon(call, true)
			return fmt.Errorf("mcp upstream: call cancelled: %w", ctx.Err())
		}
	}
}

func (u *StdioUpstream) deliverOne(ctx context.Context, call *stdioCall, queued queuedEvent, sink func(mcpwire.Event) error) error {
	// Nothing reaches the caller once it has gone.
	if err := ctx.Err(); err != nil {
		u.abandon(call, !queued.event.Terminal)
		return fmt.Errorf("mcp upstream: call cancelled: %w", err)
	}
	if err := sink(queued.event); err != nil {
		u.abandon(call, !queued.event.Terminal)
		return err
	}
	u.delivered(call)
	return nil
}

// next returns the oldest queued event and marks it in hand, or the call's
// failure once nothing is left.
func (u *StdioUpstream) next(call *stdioCall) (queuedEvent, bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(call.queue) == 0 {
		return queuedEvent{}, false, call.failed
	}
	call.inHand = true
	return call.queue[0], true, nil
}

func (u *StdioUpstream) delivered(call *stdioCall) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !call.inHand || len(call.queue) == 0 {
		return
	}
	u.budget.release(call.queue[0].size)
	call.bytes -= call.queue[0].size
	call.queue = call.queue[1:]
	call.inHand = false
}

// dropQueuedLocked releases everything the caller has not been handed, so
// a failed slow caller stops holding shared budget at once.
func (u *StdioUpstream) dropQueuedLocked(call *stdioCall) {
	keep := 0
	if call.inHand {
		keep = 1
	}
	for _, queued := range call.queue[min(keep, len(call.queue)):] {
		u.budget.release(queued.size)
		call.bytes -= queued.size
	}
	call.queue = call.queue[:min(keep, len(call.queue))]
}

// byteBudget is a shared admission counter for queued reply bytes.
// A budget with a parent also charges the parent, so the host-wide limit
// holds however many instances are queueing replies.
type byteBudget struct {
	mu     sync.Mutex
	limit  int
	used   int
	parent *byteBudget
}

func (b *byteBudget) take(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.limit {
		return false
	}
	if b.parent != nil && !b.parent.take(n) {
		return false
	}
	b.used += n
	return true
}

func (b *byteBudget) release(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
	if b.parent != nil {
		b.parent.release(n)
	}
}
