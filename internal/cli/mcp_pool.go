package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

const (
	mcpPoolShortLimit  = 8
	mcpPoolStreamLimit = 4
	mcpPoolIdleTimeout = 60 * time.Second
)

var errMCPPoolClosed = errors.New("MCP host connection pool is closed")

// preDispatchError wraps errors that occurred before a control frame was written
// (slot acquisition, dial, TLS/pin handshake, host.info, or targetName check).
type preDispatchError struct{ err error }

func (e *preDispatchError) Error() string { return e.err.Error() }
func (e *preDispatchError) Unwrap() error { return e.err }

func isPreDispatchError(err error) bool {
	var target *preDispatchError
	return errors.As(err, &target)
}

type mcpHostKey struct {
	id           string
	meshIdentity string
	endpoint     string
	local        bool
}

func makeMCPHostKey(host HostRecord) mcpHostKey {
	return mcpHostKey{
		id:           host.ID,
		meshIdentity: host.MeshIdentity,
		endpoint:     host.Endpoint,
		local:        host.local,
	}
}

type mcpPooledConn struct {
	conn   transport.Conn
	info   protocol.HostInfo
	idleAt time.Time
}

type mcpHostBucket struct {
	shortSlots  chan struct{}
	streamSlots chan struct{}

	mu   sync.Mutex
	idle []*mcpPooledConn
}

func newMCPHostBucket() *mcpHostBucket {
	return &mcpHostBucket{
		shortSlots:  make(chan struct{}, mcpPoolShortLimit),
		streamSlots: make(chan struct{}, mcpPoolStreamLimit),
	}
}

// mcpHostPool pools pinned authenticated connections per host with independent
// short-control and long-stream concurrency budgets and deadline-driven idle retirement.
type mcpHostPool struct {
	dial        HostDialer
	idleTimeout time.Duration
	now         func() time.Time

	mu      sync.Mutex
	closed  bool
	buckets map[mcpHostKey]*mcpHostBucket
	live    map[transport.Conn]struct{}

	reschedule chan struct{}
	stopSweep  chan struct{}
	sweepDone  chan struct{}
}

func newMCPHostPool(dial HostDialer, now func() time.Time) *mcpHostPool {
	return newMCPHostPoolWithIdle(dial, now, mcpPoolIdleTimeout)
}

func newMCPHostPoolWithIdle(dial HostDialer, now func() time.Time, idleTimeout time.Duration) *mcpHostPool {
	if dial == nil {
		dial = dialAgentHost
	}
	if now == nil {
		now = time.Now
	}
	if idleTimeout <= 0 {
		idleTimeout = mcpPoolIdleTimeout
	}
	p := &mcpHostPool{
		dial:        dial,
		idleTimeout: idleTimeout,
		now:         now,
		buckets:     make(map[mcpHostKey]*mcpHostBucket),
		live:        make(map[transport.Conn]struct{}),
		reschedule:  make(chan struct{}, 1),
		stopSweep:   make(chan struct{}),
		sweepDone:   make(chan struct{}),
	}
	go p.sweepLoop()
	return p
}

// Close immediately closes all idle and in-flight host connections.
func (p *mcpHostPool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	conns := p.drainAllConnectionsLocked()
	p.mu.Unlock()

	close(p.stopSweep)
	for _, conn := range conns {
		_ = conn.Close()
	}
	<-p.sweepDone
}

func (p *mcpHostPool) drainAllConnectionsLocked() []transport.Conn {
	conns := make([]transport.Conn, 0, len(p.live))
	for conn := range p.live {
		conns = append(conns, conn)
	}
	for _, bucket := range p.buckets {
		bucket.mu.Lock()
		for _, entry := range bucket.idle {
			conns = append(conns, entry.conn)
		}
		bucket.idle = nil
		bucket.mu.Unlock()
	}
	p.live = make(map[transport.Conn]struct{})
	return conns
}

// ExchangeShort sends one short control request using the host's 8-slot short budget.
// When retryRead is true, a stale idle connection is redialed once before returning an error.
func (p *mcpHostPool) ExchangeShort(ctx context.Context, host HostRecord, req protocol.Control, retryRead bool) (protocol.Control, protocol.HostInfo, error) {
	return p.exchangeShort(ctx, host, req, retryRead)
}

// ExchangeStreamBudgeted acquires a stream slot and pooled connection first,
// then computes the remaining host wait timeout against deadline so slot queuing
// or a cold dial does not exhaust the client context before the host replies.
func (p *mcpHostPool) ExchangeStreamBudgeted(ctx context.Context, host HostRecord, deadline time.Time, build func(timeoutMS int) protocol.Control) (protocol.Control, protocol.HostInfo, error) {
	bucket, err := p.bucket(host)
	if err != nil {
		return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: err}
	}
	if err := acquireSlot(ctx, bucket.streamSlots); err != nil {
		return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: err}
	}
	defer func() { <-bucket.streamSlots }()

	entry, _, err := p.checkout(ctx, bucket, host)
	if err != nil {
		return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: err}
	}
	remaining := time.Until(deadline)
	timeoutMS := max(1, int(remaining/time.Millisecond))
	req := build(timeoutMS)
	if req.RequestID == "" {
		req.RequestID, err = newDaemonRequestID()
		if err != nil {
			p.checkin(bucket, entry)
			return protocol.Control{}, entry.info, &preDispatchError{err: err}
		}
	}
	rpcCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond+5*time.Second)
	defer cancel()
	resp, err := controlRequest(rpcCtx, entry.conn, req)
	if err != nil {
		p.discard(entry.conn)
		return protocol.Control{}, entry.info, err
	}
	p.checkin(bucket, entry)
	return resp, entry.info, nil
}

// HostInfo probes or reuses a pinned connection to read host.info capabilities.
func (p *mcpHostPool) HostInfo(ctx context.Context, host HostRecord) (protocol.HostInfo, error) {
	bucket, err := p.bucket(host)
	if err != nil {
		return protocol.HostInfo{}, &preDispatchError{err: err}
	}
	if err := acquireSlot(ctx, bucket.shortSlots); err != nil {
		return protocol.HostInfo{}, &preDispatchError{err: err}
	}
	defer func() { <-bucket.shortSlots }()

	entry, reused, err := p.checkout(ctx, bucket, host)
	if err != nil {
		return protocol.HostInfo{}, &preDispatchError{err: err}
	}
	if !reused {
		p.checkin(bucket, entry)
		return entry.info, nil
	}
	reqID, err := newDaemonRequestID()
	if err != nil {
		p.discard(entry.conn)
		return protocol.HostInfo{}, &preDispatchError{err: err}
	}
	resp, err := controlRequest(ctx, entry.conn, protocol.Control{Type: protocol.TypeHostInfo, RequestID: reqID})
	if err != nil {
		p.discard(entry.conn)
		fresh, _, dialErr := p.checkoutFresh(ctx, host)
		if dialErr != nil {
			return protocol.HostInfo{}, &preDispatchError{err: dialErr}
		}
		p.checkin(bucket, fresh)
		return fresh.info, nil
	}
	if err := validateAgentHostInfo(host, resp); err != nil {
		p.discard(entry.conn)
		return protocol.HostInfo{}, &preDispatchError{err: err}
	}
	entry.info = *resp.Host
	p.checkin(bucket, entry)
	return entry.info, nil
}

// HostContext reads host.context through the host's pooled short-control budget,
// returning both the parsed context reply and validated HostInfo.
func (p *mcpHostPool) HostContext(ctx context.Context, host HostRecord, full bool) (hostprofile.ContextReply, *protocol.HostInfo, error) {
	data, err := json.Marshal(hostprofile.ContextRequest{Full: full})
	if err != nil {
		return hostprofile.ContextReply{}, nil, fmt.Errorf("encode host context request: %w", err)
	}
	response, info, err := p.ExchangeShort(ctx, host, protocol.Control{Type: protocol.TypeHostContext, Context: data}, true)
	if err != nil {
		if info.ID != "" {
			return hostprofile.ContextReply{}, &info, err
		}
		return hostprofile.ContextReply{}, nil, err
	}
	if err := validateContextObservation(info); err != nil {
		return hostprofile.ContextReply{}, nil, err
	}
	if response.Type == protocol.TypeError {
		return hostprofile.ContextReply{}, &info, daemonResponseError("host context", response.Message, response.ErrorCode)
	}
	if response.Type != protocol.TypeHostContextResult {
		return hostprofile.ContextReply{}, &info, errors.New("host did not return host context")
	}
	reply, err := hostprofile.ParseContext(response.Context, host.ID)
	if err != nil {
		return hostprofile.ContextReply{}, &info, fmt.Errorf("validate pinned host context: %w", err)
	}
	if full && (reply.Host.Completeness != hostprofile.Full || reply.Fleet.Completeness != hostprofile.Full) {
		return hostprofile.ContextReply{}, &info, errors.New("host returned incomplete context for inspect")
	}
	return reply, &info, nil
}

func (p *mcpHostPool) exchangeShort(ctx context.Context, host HostRecord, req protocol.Control, retryRead bool) (protocol.Control, protocol.HostInfo, error) {
	bucket, err := p.bucket(host)
	if err != nil {
		return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: err}
	}
	if err := acquireSlot(ctx, bucket.shortSlots); err != nil {
		return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: err}
	}
	defer func() { <-bucket.shortSlots }()

	if req.RequestID == "" {
		req.RequestID, err = newDaemonRequestID()
		if err != nil {
			return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: err}
		}
	}
	entry, reused, err := p.checkout(ctx, bucket, host)
	if err != nil {
		return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: err}
	}
	resp, err := controlRequest(ctx, entry.conn, req)
	if err == nil {
		p.checkin(bucket, entry)
		return resp, entry.info, nil
	}
	p.discard(entry.conn)
	if !reused || !retryRead || ctx.Err() != nil {
		return protocol.Control{}, entry.info, err
	}
	fresh, _, dialErr := p.checkoutFresh(ctx, host)
	if dialErr != nil {
		return protocol.Control{}, protocol.HostInfo{}, &preDispatchError{err: dialErr}
	}
	resp, err = controlRequest(ctx, fresh.conn, req)
	if err != nil {
		p.discard(fresh.conn)
		return protocol.Control{}, fresh.info, err
	}
	p.checkin(bucket, fresh)
	return resp, fresh.info, nil
}

func (p *mcpHostPool) bucket(host HostRecord) (*mcpHostBucket, error) {
	key := makeMCPHostKey(host)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errMCPPoolClosed
	}
	b := p.buckets[key]
	if b == nil {
		b = newMCPHostBucket()
		p.buckets[key] = b
	}
	return b, nil
}

func (p *mcpHostPool) checkout(ctx context.Context, bucket *mcpHostBucket, host HostRecord) (*mcpPooledConn, bool, error) {
	if entry := p.popIdle(bucket); entry != nil {
		if host.targetName != "" && host.targetName != entry.info.MachineName {
			p.checkin(bucket, entry)
			return nil, false, &hostIdentityError{label: HostLabel(host)}
		}
		return entry, true, nil
	}
	return p.checkoutFresh(ctx, host)
}

func (p *mcpHostPool) checkoutFresh(ctx context.Context, host HostRecord) (*mcpPooledConn, bool, error) {
	conn, info, err := openAgentHostInfo(ctx, host, p.dial)
	if err != nil {
		return nil, false, err
	}
	if !p.trackLive(conn) {
		_ = conn.Close()
		return nil, false, errMCPPoolClosed
	}
	return &mcpPooledConn{conn: conn, info: info}, false, nil
}

func (p *mcpHostPool) popIdle(bucket *mcpHostBucket) *mcpPooledConn {
	now := p.now()
	bucket.mu.Lock()
	var expired []*mcpPooledConn
	var chosen *mcpPooledConn
	for len(bucket.idle) > 0 {
		last := bucket.idle[len(bucket.idle)-1]
		bucket.idle[len(bucket.idle)-1] = nil
		bucket.idle = bucket.idle[:len(bucket.idle)-1]
		if now.Sub(last.idleAt) >= p.idleTimeout {
			expired = append(expired, last)
			continue
		}
		chosen = last
		break
	}
	bucket.mu.Unlock()
	for _, entry := range expired {
		p.discard(entry.conn)
	}
	return chosen
}

func (p *mcpHostPool) checkin(bucket *mcpHostBucket, entry *mcpPooledConn) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		p.discard(entry.conn)
		return
	}
	entry.idleAt = p.now()
	bucket.mu.Lock()
	if len(bucket.idle) >= mcpPoolShortLimit {
		bucket.mu.Unlock()
		p.discard(entry.conn)
		return
	}
	bucket.idle = append(bucket.idle, entry)
	bucket.mu.Unlock()
	select {
	case p.reschedule <- struct{}{}:
	default:
	}
}

func (p *mcpHostPool) trackLive(conn transport.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.live[conn] = struct{}{}
	return true
}

func (p *mcpHostPool) discard(conn transport.Conn) {
	p.mu.Lock()
	delete(p.live, conn)
	p.mu.Unlock()
	_ = conn.Close()
}

func (p *mcpHostPool) sweepLoop() {
	defer close(p.sweepDone)
	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
			timerC = nil
		}
	}
	defer stopTimer()
	for {
		stopTimer()
		if delay, hasIdle := p.expireAndNextDelay(); hasIdle {
			timer = time.NewTimer(delay)
			timerC = timer.C
		}
		select {
		case <-p.stopSweep:
			return
		case <-p.reschedule:
		case <-timerC:
		}
	}
}

func (p *mcpHostPool) expireAndNextDelay() (time.Duration, bool) {
	now := p.now()
	p.mu.Lock()
	buckets := make([]*mcpHostBucket, 0, len(p.buckets))
	for _, b := range p.buckets {
		buckets = append(buckets, b)
	}
	p.mu.Unlock()

	var earliest time.Time
	for _, b := range buckets {
		closing, next := expireBucket(b, now, p.idleTimeout)
		for _, conn := range closing {
			p.discard(conn)
		}
		if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
			earliest = next
		}
	}
	if earliest.IsZero() {
		return 0, false
	}
	delay := max(earliest.Sub(now), time.Millisecond)
	return delay, true
}

func expireBucket(b *mcpHostBucket, now time.Time, idleTimeout time.Duration) ([]transport.Conn, time.Time) {
	var closing []transport.Conn
	var earliest time.Time
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := b.idle[:0]
	for _, entry := range b.idle {
		deadline := entry.idleAt.Add(idleTimeout)
		if !deadline.After(now) {
			closing = append(closing, entry.conn)
			continue
		}
		kept = append(kept, entry)
		if earliest.IsZero() || deadline.Before(earliest) {
			earliest = deadline
		}
	}
	for i := len(kept); i < len(b.idle); i++ {
		b.idle[i] = nil
	}
	b.idle = kept
	return closing, earliest
}

func acquireSlot(ctx context.Context, slots chan struct{}) error {
	select {
	case slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for host connection slot: %w", ctx.Err())
	}
}
