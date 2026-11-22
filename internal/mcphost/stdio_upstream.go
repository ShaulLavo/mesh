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
}

func newStdioUpstream(conn LineConn, ids *hostIDs) *StdioUpstream {
	ctx, cancel := context.WithCancel(context.Background())
	u := &StdioUpstream{conn: conn, ids: ids, budget: &byteBudget{limit: instanceQueueBytes}, ctx: ctx, cancel: cancel, calls: make(map[string]*stdioCall), progress: make(map[string]*stdioCall), done: make(chan struct{})}
	go u.readLoop()
	return u
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
	u.fault(fmt.Errorf("mcp upstream: closed: %w", ErrNotSent))
	return nil
}

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
		if err != nil {
			u.fault(fmt.Errorf("%w: %w", ErrProtocolFault, err))
			return
		}
		if err := u.route(line); err != nil {
			u.fault(err)
			return
		}
	}
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
	if last := len(call.queue) - 1; last >= 0 && coalescible(call.queue[last].event, event) {
		u.budget.release(call.queue[last].size)
		call.bytes -= call.queue[last].size
		call.queue = call.queue[:last]
	}
	if call.bytes+size > callQueueBytes || !u.budget.take(size) {
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
	return !queued.Terminal && !next.Terminal && queued.Message.Method() == "notifications/progress" && next.Message.Method() == "notifications/progress"
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
		go u.sendCancel(call.id)
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

func (u *StdioUpstream) sendCancel(id string) {
	raw, err := encodeJSON(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id}})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(u.ctx, cancelWriteTimeout)
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
	if err := u.conn.WriteLine(u.ctx, line); err != nil {
		u.abandon(call, false)
		return err
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
	token, _ := decodeJSONString(member(request.Raw(), "params", "_meta", "progressToken"))
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
	call.queue, call.bytes = nil, 0
	u.mu.Unlock()
	if active && sent {
		go u.sendCancel(call.id)
	}
}

func (u *StdioUpstream) drain(ctx context.Context, call *stdioCall, sink func(mcpwire.Event) error) error {
	for {
		events, failed := u.take(call)
		for i, queued := range events {
			if err := sink(queued.event); err != nil {
				u.abandon(call, !events[i].event.Terminal)
				return err
			}
			if queued.event.Terminal {
				return nil
			}
		}
		if failed != nil {
			u.abandon(call, false)
			return failed
		}
		if len(events) > 0 {
			continue
		}
		select {
		case <-call.signal:
		case <-ctx.Done():
			u.abandon(call, true)
			return fmt.Errorf("mcp upstream: call cancelled: %w", ctx.Err())
		}
	}
}

func (u *StdioUpstream) take(call *stdioCall) ([]queuedEvent, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	events := call.queue
	call.queue, call.bytes = nil, 0
	for _, queued := range events {
		u.budget.release(queued.size)
	}
	if len(events) > 0 {
		return events, nil
	}
	return nil, call.failed
}

// byteBudget is a shared admission counter for queued reply bytes.
type byteBudget struct {
	mu    sync.Mutex
	limit int
	used  int
}

func (b *byteBudget) take(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.limit {
		return false
	}
	b.used += n
	return true
}

func (b *byteBudget) release(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
}
