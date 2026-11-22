package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

const (
	mcpPoolShortLimit  = 8
	mcpPoolStreamLimit = 4
	mcpPoolIdleTimeout = 60 * time.Second
	mcpPoolSweepPeriod = 15 * time.Second
)

var errMCPPoolClosed = errors.New("MCP host connection pool is closed")

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
// short-control and long-stream concurrency budgets and 60-second idle retirement.
type mcpHostPool struct {
	dial        HostDialer
	idleTimeout time.Duration
	now         func() time.Time

	mu      sync.Mutex
	closed  bool
	buckets map[mcpHostKey]*mcpHostBucket
	live    map[transport.Conn]struct{}

	stopSweep chan struct{}
	sweepDone chan struct{}
}

func newMCPHostPool(dial HostDialer, now func() time.Time) *mcpHostPool {
	if dial == nil {
		dial = dialAgentHost
	}
	if now == nil {
		now = time.Now
	}
	p := &mcpHostPool{
		dial:        dial,
		idleTimeout: mcpPoolIdleTimeout,
		now:         now,
		buckets:     make(map[mcpHostKey]*mcpHostBucket),
		live:        make(map[transport.Conn]struct{}),
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
	p.mu.Unlock()

	close(p.stopSweep)
	for _, conn := range conns {
		_ = conn.Close()
	}
	<-p.sweepDone
}

// ExchangeShort sends one short control request using the host's 8-slot short budget.
// When retryRead is true, a stale idle connection is redialed once before returning an error.
func (p *mcpHostPool) ExchangeShort(ctx context.Context, host HostRecord, req protocol.Control, retryRead bool) (protocol.Control, protocol.HostInfo, error) {
	return p.exchange(ctx, host, req, false, retryRead)
}

// ExchangeStream sends one long-running control (such as execution.wait) using
// the host's separate 4-slot stream budget so waits never starve short controls.
func (p *mcpHostPool) ExchangeStream(ctx context.Context, host HostRecord, req protocol.Control) (protocol.Control, protocol.HostInfo, error) {
	return p.exchange(ctx, host, req, true, false)
}

// HostInfo probes or reuses a pinned connection to read host.info capabilities.
func (p *mcpHostPool) HostInfo(ctx context.Context, host HostRecord) (protocol.HostInfo, error) {
	bucket, err := p.bucket(host)
	if err != nil {
		return protocol.HostInfo{}, err
	}
	if err := acquireSlot(ctx, bucket.shortSlots); err != nil {
		return protocol.HostInfo{}, err
	}
	defer func() { <-bucket.shortSlots }()

	entry, reused, err := p.checkout(ctx, bucket, host)
	if err != nil {
		return protocol.HostInfo{}, err
	}
	if !reused {
		p.checkin(bucket, entry)
		return entry.info, nil
	}
	reqID, err := newDaemonRequestID()
	if err != nil {
		p.discard(entry.conn)
		return protocol.HostInfo{}, err
	}
	resp, err := controlRequest(ctx, entry.conn, protocol.Control{Type: protocol.TypeHostInfo, RequestID: reqID})
	if err != nil {
		p.discard(entry.conn)
		fresh, _, dialErr := p.checkoutFresh(ctx, host)
		if dialErr != nil {
			return protocol.HostInfo{}, dialErr
		}
		p.checkin(bucket, fresh)
		return fresh.info, nil
	}
	if err := validateAgentHostInfo(host, resp); err != nil {
		p.discard(entry.conn)
		return protocol.HostInfo{}, err
	}
	entry.info = *resp.Host
	p.checkin(bucket, entry)
	return entry.info, nil
}

func (p *mcpHostPool) exchange(ctx context.Context, host HostRecord, req protocol.Control, stream, retryRead bool) (protocol.Control, protocol.HostInfo, error) {
	bucket, err := p.bucket(host)
	if err != nil {
		return protocol.Control{}, protocol.HostInfo{}, err
	}
	slots := bucket.shortSlots
	if stream {
		slots = bucket.streamSlots
	}
	if err := acquireSlot(ctx, slots); err != nil {
		return protocol.Control{}, protocol.HostInfo{}, err
	}
	defer func() { <-slots }()

	if req.RequestID == "" {
		req.RequestID, err = newDaemonRequestID()
		if err != nil {
			return protocol.Control{}, protocol.HostInfo{}, err
		}
	}
	entry, reused, err := p.checkout(ctx, bucket, host)
	if err != nil {
		return protocol.Control{}, protocol.HostInfo{}, err
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
		return protocol.Control{}, protocol.HostInfo{}, dialErr
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
	ticker := time.NewTicker(mcpPoolSweepPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopSweep:
			return
		case <-ticker.C:
			p.expireIdle()
		}
	}
}

func (p *mcpHostPool) expireIdle() {
	now := p.now()
	p.mu.Lock()
	buckets := make([]*mcpHostBucket, 0, len(p.buckets))
	for _, b := range p.buckets {
		buckets = append(buckets, b)
	}
	p.mu.Unlock()

	for _, b := range buckets {
		var closing []transport.Conn
		b.mu.Lock()
		kept := b.idle[:0]
		for _, entry := range b.idle {
			if now.Sub(entry.idleAt) >= p.idleTimeout {
				closing = append(closing, entry.conn)
				continue
			}
			kept = append(kept, entry)
		}
		b.idle = kept
		b.mu.Unlock()
		for _, conn := range closing {
			p.discard(conn)
		}
	}
}

func acquireSlot(ctx context.Context, slots chan struct{}) error {
	select {
	case slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for host connection slot: %w", ctx.Err())
	}
}
