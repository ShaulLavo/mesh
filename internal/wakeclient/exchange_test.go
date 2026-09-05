package wakeclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/wake"
)

func TestExchangePinsIdentityBeforeSendingMutation(t *testing.T) {
	actual := fixtureGrant(t, true)
	expected := fixtureGrant(t, true)
	var mutations atomic.Int32
	endpoint := serveTestPeer(t, func(_ context.Context, request protocol.Control) (protocol.Control, error) {
		if request.Type == protocol.TypeHostInfo {
			return testHostInfo(request, actual.TargetID, "peer", nil), nil
		}
		mutations.Add(1)
		return protocol.Control{Type: protocol.TypeWakeSent}, nil
	})
	_, _, err := exchange(testContext(t), endpoint, expected.TargetID, protocol.Control{Type: protocol.TypeWakeSend, WakeGrant: &expected})
	if err == nil || !strings.Contains(err.Error(), "identity changed") || mutations.Load() != 0 {
		t.Fatalf("exchange = %v, mutations = %d", err, mutations.Load())
	}
}

func TestWakeRejectsWrongTargetIdentityBeforeSending(t *testing.T) {
	client, _, target := rememberedClient(t)
	foreign := fixtureGrant(t, true)
	target.Endpoint = serveTestPeer(t, func(_ context.Context, request protocol.Control) (protocol.Control, error) {
		return testHostInfo(request, foreign.TargetID, "another-machine", nil), nil
	})
	var sends atomic.Int32
	client.sender = stubSender{
		probe: func(context.Context, wake.Grant) (wake.State, error) { return wake.Down, nil },
		send:  func(context.Context, wake.Grant) (bool, error) { sends.Add(1); return true, nil },
	}
	_, err := client.Wake(testContext(t), target)
	if !errors.Is(err, ErrIdentityChanged) || sends.Load() != 0 {
		t.Fatalf("Wake = %v, sends = %d", err, sends.Load())
	}
}

func TestExchangeRejectsResponseForAnotherRequest(t *testing.T) {
	grant := fixtureGrant(t, true)
	endpoint := serveTestPeer(t, func(_ context.Context, request protocol.Control) (protocol.Control, error) {
		if request.Type == protocol.TypeHostInfo {
			return testHostInfo(request, grant.TargetID, "peer", nil), nil
		}
		return protocol.Control{Type: protocol.TypeWakeSent, RequestID: "another-request"}, nil
	})
	_, _, err := exchange(testContext(t), endpoint, grant.TargetID, protocol.Control{Type: protocol.TypeWakeSend, WakeGrant: &grant})
	if err == nil || !strings.Contains(err.Error(), "request ID") {
		t.Fatalf("exchange = %v", err)
	}
}

func TestExchangeCancellationClosesBlockedWebSocket(t *testing.T) {
	grant := fixtureGrant(t, true)
	received := make(chan struct{})
	closed := make(chan struct{})
	endpoint := serveTestPeer(t, func(ctx context.Context, _ protocol.Control) (protocol.Control, error) {
		close(received)
		<-ctx.Done()
		close(closed)
		return protocol.Control{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	done := make(chan struct{})
	go func() {
		_, _, err := exchange(ctx, endpoint, grant.TargetID, protocol.Control{})
		if err == nil {
			t.Error("cancelled exchange returned success")
		}
		close(done)
	}()
	waitSignal(t, received)
	cancel()
	waitSignal(t, done)
	waitSignal(t, closed)
}

func serveTestPeer(t *testing.T, handle func(context.Context, protocol.Control) (protocol.Control, error)) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.Serve(w, r, func(ctx context.Context, conn transport.Conn) error {
			return serveTestControls(ctx, conn, handle)
		})
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/mesh"
}

func serveTestControls(ctx context.Context, conn transport.Conn, handle func(context.Context, protocol.Control) (protocol.Control, error)) error {
	for {
		frame, err := conn.ReadFrame()
		if err != nil {
			return err
		}
		if frame.Kind != protocol.KindControl {
			return errors.New("test peer expected control frame")
		}
		request, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return err
		}
		response, err := handle(ctx, request)
		if err != nil {
			return err
		}
		if response.RequestID == "" {
			response.RequestID = request.RequestID
		}
		payload, err := response.Encode()
		if err != nil {
			return err
		}
		if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
			return err
		}
	}
}

func testHostInfo(request protocol.Control, id, name string, grant *wake.Grant) protocol.Control {
	return protocol.Control{Type: protocol.TypeHostInfoResult, RequestID: request.RequestID, Host: &protocol.HostInfo{
		ID: id, MeshIdentity: id, TailscaleName: name, Wake: grant,
	}}
}
