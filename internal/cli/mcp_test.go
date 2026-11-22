package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

type poolFixtureConn struct {
	mu      sync.Mutex
	closed  bool
	handler func(protocol.Control) (protocol.Control, error)
	replies chan protocol.Frame
	done    chan struct{}
}

func newPoolFixtureConn(handler func(protocol.Control) (protocol.Control, error)) *poolFixtureConn {
	return &poolFixtureConn{
		handler: handler,
		replies: make(chan protocol.Frame, 4),
		done:    make(chan struct{}),
	}
}

func (c *poolFixtureConn) WriteFrame(frame protocol.Frame) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return io.EOF
	}
	c.mu.Unlock()
	req, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("decode fixture control: %w", err)
	}
	resp, err := c.handler(req)
	if err != nil {
		return err
	}
	resp.RequestID = req.RequestID
	payload, err := resp.Encode()
	if err != nil {
		return fmt.Errorf("encode fixture control: %w", err)
	}
	select {
	case c.replies <- protocol.Frame{Kind: protocol.KindControl, Payload: payload}:
		return nil
	case <-c.done:
		return io.EOF
	}
}

func (c *poolFixtureConn) ReadFrame() (protocol.Frame, error) {
	select {
	case frame := <-c.replies:
		return frame, nil
	case <-c.done:
		return protocol.Frame{}, io.EOF
	}
}

func (c *poolFixtureConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	return nil
}

func TestMCPHostPoolReuseSeparateBudgetsAndClose(t *testing.T) {
	host := HostRecord{
		ID:           "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE",
		MeshIdentity: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE",
		MachineName:  "pc",
		NameRevision: 1,
		Endpoint:     "ws://127.0.0.1:1/control/ws",
	}

	var dials atomic.Int32
	streamEntered := make(chan struct{}, mcpPoolStreamLimit)
	releaseStreams := make(chan struct{})

	dial := func(_ context.Context, h HostRecord) (transport.Conn, error) {
		dials.Add(1)
		return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
			switch req.Type {
			case protocol.TypeHostInfo:
				return protocol.Control{
					Type: protocol.TypeHostInfoResult,
					Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: h.MachineName, NameRevision: h.NameRevision, ExecutionPipeSupported: true},
				}, nil
			case protocol.TypeExecutionWait:
				streamEntered <- struct{}{}
				<-releaseStreams
				return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionWait: "timed_out"}, nil
			default:
				return protocol.Control{Type: protocol.TypeListed}, nil
			}
		}), nil
	}

	pool := newMCPHostPool(dial, time.Now)
	defer pool.Close()

	// Sequential short controls reuse the same connection (1 dial total).
	for range 3 {
		resp, info, err := pool.ExchangeShort(t.Context(), host, protocol.Control{Type: protocol.TypeList}, true)
		if err != nil || resp.Type != protocol.TypeListed || !info.ExecutionPipeSupported {
			t.Fatalf("short exchange failed: resp=%+v info=%+v err=%v", resp, info, err)
		}
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want 1 (connection reuse)", dials.Load())
	}

	// Fill all 4 stream slots; short controls must still succeed immediately.
	var wg sync.WaitGroup
	for range mcpPoolStreamLimit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := pool.ExchangeStreamBudgeted(t.Context(), host, time.Now().Add(5*time.Second), func(timeoutMS int) protocol.Control {
				return protocol.Control{
					Type:          protocol.TypeExecutionWait,
					SessionID:     "7K3D",
					Instance:      "0123456789abcdef0123456789abcdef",
					TimeoutMillis: timeoutMS,
				}
			})
			if err != nil {
				t.Errorf("stream exchange failed: %v", err)
			}
		}()
	}
	for range mcpPoolStreamLimit {
		<-streamEntered
	}

	shortCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if resp, _, err := pool.ExchangeShort(shortCtx, host, protocol.Control{Type: protocol.TypeList}, true); err != nil || resp.Type != protocol.TypeListed {
		t.Fatalf("short control blocked by full stream slots: resp=%+v err=%v", resp, err)
	}

	close(releaseStreams)
	wg.Wait()
}

func TestAgentErrorMapsDestinationIdentityMismatch(t *testing.T) {
	err := errors.Join(transport.ErrAuthentication, transport.ErrDestinationIdentityMismatch)
	got := agentError(err, "sessions.inspect", "host-1", agentapi.HostUnreachable)
	if got.Code != agentapi.HostIdentityMismatch {
		t.Fatalf("code = %s, want %s", got.Code, agentapi.HostIdentityMismatch)
	}
}

func TestMCPHostPoolShortCeilingIdleDeadlineAndTargetNameReuse(t *testing.T) {
	hostID := "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"
	host := HostRecord{
		ID:           hostID,
		MeshIdentity: hostID,
		MachineName:  "pc",
		NameRevision: 1,
		Endpoint:     "ws://127.0.0.1:1/control/ws",
	}

	var active atomic.Int32
	var maxActive atomic.Int32
	var closed atomic.Int32
	var zeroRev uint64
	ctxReplyBytes, _ := json.Marshal(hostprofile.ContextReply{
		Host:  hostprofile.WireSection{State: hostprofile.StateAbsent, Completeness: hostprofile.Full, Revision: &zeroRev},
		Fleet: hostprofile.WireSection{State: hostprofile.StateAbsent, Completeness: hostprofile.Full, Revision: &zeroRev},
	})

	dial := func(_ context.Context, h HostRecord) (transport.Conn, error) {
		fc := newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
			if req.Type == protocol.TypeHostInfo {
				return protocol.Control{
					Type: protocol.TypeHostInfoResult,
					Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: "other", NameRevision: 1, Account: "remote-acct", ExecutionPipeSupported: true},
				}, nil
			}
			cur := active.Add(1)
			for {
				prev := maxActive.Load()
				if cur <= prev || maxActive.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(25 * time.Millisecond)
			active.Add(-1)
			return protocol.Control{Type: protocol.TypeHostContextResult, Context: ctxReplyBytes}, nil
		})
		return &closeCountingConn{Conn: fc, closed: &closed}, nil
	}

	pool := newMCPHostPoolWithIdle(dial, time.Now, 80*time.Millisecond)
	defer pool.Close()

	// 1. Short ceiling: 16 concurrent HostContext calls never exceed mcpPoolShortLimit (8).
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := pool.HostContext(t.Context(), host, true); err != nil {
				t.Errorf("HostContext failed: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := maxActive.Load(); got > mcpPoolShortLimit {
		t.Fatalf("concurrent short calls = %d, exceeded limit %d", got, mcpPoolShortLimit)
	}

	// 2. Named checkout reusing an exact-ID pooled connection checks targetName.
	namedHost := host
	namedHost.targetName = "pc"
	if _, _, err := pool.ExchangeShort(t.Context(), namedHost, protocol.Control{Type: protocol.TypeList}, false); err == nil {
		t.Fatal("expected targetName mismatch on pooled reuse")
	}

	// 3. Idle connections close right at idleTimeout with NO subsequent caller.
	deadline := time.Now().Add(600 * time.Millisecond)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}
	if closed.Load() == 0 {
		t.Fatal("idle pooled connections did not close at idleTimeout without a subsequent caller")
	}
}

type closeCountingConn struct {
	transport.Conn
	once   sync.Once
	closed *atomic.Int32
}

func (c *closeCountingConn) Close() error {
	c.once.Do(func() { c.closed.Add(1) })
	if err := c.Conn.Close(); err != nil {
		return fmt.Errorf("close pooled test conn: %w", err)
	}
	return nil
}

func TestMCPExactHostIDPreDispatchInputErrorRemoteAccountAndQueuedWait(t *testing.T) {
	prepareCommandEnvironment(t)
	hostID := "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"
	host := HostRecord{
		ID:           hostID,
		MeshIdentity: hostID,
		MachineName:  "pc",
		NameRevision: 1,
		Endpoint:     "ws://127.0.0.1:1/control/ws",
	}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}

	app := &application{dependencies: Dependencies{Now: time.Now}}

	// 1. resolveExactHostID rejects machine name "pc" even though "pc" exists in hosts.json.
	controls := newMCPControls(app, newMCPHostPool(nil, time.Now), "")
	defer controls.pool.Close()
	if _, err := controls.resolveExactHostID("pc"); err == nil {
		t.Fatal("resolveExactHostID accepted machine name 'pc'")
	}
	if got, err := controls.resolveExactHostID(hostID); err != nil || got.ID != hostID {
		t.Fatalf("resolveExactHostID(%q) = %+v, %v", hostID, got, err)
	}

	// 2. Pre-dispatch identity mismatch on keyed JobInput returns host.identity_mismatch (not input.outcome_unknown).
	mismatchPool := newMCPHostPool(func(context.Context, HostRecord) (transport.Conn, error) {
		return nil, errors.Join(transport.ErrAuthentication, transport.ErrDestinationIdentityMismatch)
	}, time.Now)
	defer mismatchPool.Close()
	mismatchControls := newMCPControls(app, mismatchPool, "")
	offset := uint64(0)
	out, err := mismatchControls.JobInput(t.Context(), mcpnative.JobInputRequest{
		Job:      mcpnative.JobRef{HostID: hostID, SessionID: "7K3D", Instance: "0123456789abcdef0123456789abcdef"},
		WriteKey: "w1",
		Offset:   &offset,
		Data:     []byte("hi"),
		Timeout:  2 * time.Second,
	})
	if err != nil || !out.IsError || len(out.Envelope.Errors) == 0 || out.Envelope.Errors[0].Code != agentapi.HostIdentityMismatch {
		t.Fatalf("expected host.identity_mismatch on pre-dispatch input failure, got out=%+v err=%v", out, err)
	}

	// 3. Remote target omits frontend account when host omits Account, and reports remote Account when present.
	// 4. Slow dial (650ms > old 500ms margin) with WaitMS=300 returns timed_out with receipt, not host.unreachable.
	slowDial := func(_ context.Context, h HostRecord) (transport.Conn, error) {
		time.Sleep(650 * time.Millisecond)
		return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
			if req.Type == protocol.TypeHostInfo {
				return protocol.Control{
					Type: protocol.TypeHostInfoResult,
					Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: "pc", NameRevision: 1, Account: "remote-bob", ExecutionPipeSupported: true},
				}, nil
			}
			return protocol.Control{
				Type:          protocol.TypeExecutionReceipt,
				ExecutionWait: "timed_out",
				ExecutionReceipt: &protocol.ExecutionReceipt{
					HostID: h.ID, SessionID: req.SessionID, Instance: req.Instance, RequestKey: "k1",
					KeyScope: "device", Kind: "pipe", Phase: "live", Acceptance: "durable", LaunchOutcome: "started", Publication: "published", Removal: "present",
				},
			}, nil
		}), nil
	}
	slowPool := newMCPHostPool(slowDial, time.Now)
	defer slowPool.Close()
	slowControls := newMCPControls(app, slowPool, "")
	waitOut, err := slowControls.JobWait(t.Context(), mcpnative.JobWaitRequest{
		Job:    mcpnative.JobRef{HostID: hostID, SessionID: "7K3D", Instance: "0123456789abcdef0123456789abcdef"},
		WaitMS: 300,
	}, nil)
	if err != nil || waitOut.IsError || waitOut.Target == nil || waitOut.Target.Account != "remote-bob" {
		t.Fatalf("queued/cold wait failed or wrong remote account: out=%+v err=%v", waitOut, err)
	}
	stateData, ok := waitOut.Envelope.Data.(mcpnative.JobStateData)
	if !ok || stateData.Wait != "timed_out" {
		t.Fatalf("expected timed_out JobStateData, got %#v", waitOut.Envelope.Data)
	}

	// Remote host without Account omits target.Account (never stamps frontend account).
	if got := slowControls.targetAccount(host, protocol.HostInfo{}); got != "" {
		t.Fatalf("expected empty remote account when host omits it, got %q", got)
	}
}
