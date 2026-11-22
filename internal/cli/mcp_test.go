package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
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
			_, _, err := pool.ExchangeStream(t.Context(), host, protocol.Control{
				Type:          protocol.TypeExecutionWait,
				SessionID:     "7K3D",
				Instance:      "0123456789abcdef0123456789abcdef",
				TimeoutMillis: 5000,
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
