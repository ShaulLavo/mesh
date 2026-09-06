package daemon

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/tunnel"
)

type tunnelRecoveryProbe struct {
	recovered atomic.Int32
	claims    atomic.Int32
}

func (p *tunnelRecoveryProbe) RecoverTunnelClaim(context.Context, string) error {
	p.recovered.Add(1)
	return nil
}
func (p *tunnelRecoveryProbe) HandleControl(_ context.Context, request protocol.Control) (protocol.Control, bool, error) {
	if request.Type != protocol.TypeTunnelClaim {
		return protocol.Control{}, false, nil
	}
	p.claims.Add(1)
	return protocol.Control{Type: protocol.TypeTunnelClaimed, RequestID: request.RequestID}, true, nil
}

func TestTunnelRecoveryRequiresUnixTrustMarker(t *testing.T) {
	t.Run("tailnet", func(t *testing.T) { testTunnelRecoveryBoundary(t, false) })
	t.Run("unix", func(t *testing.T) { testTunnelRecoveryBoundary(t, true) })
}

func testTunnelRecoveryBoundary(t *testing.T, local bool) {
	t.Helper()
	probe := &tunnelRecoveryProbe{}
	server, err := newClientServer(mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector()), failingServerTestConnector(), probe, noServiceControl{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if local {
		ctx = context.WithValue(ctx, localClientKey{}, true)
	}
	client := newServerTestConn()
	done := make(chan error, 1)
	go func() { done <- server.Handle(ctx, client) }()
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeTunnelRecover, RequestID: "recover", TunnelName: "blog.shaulavo.dev"}))
	response := decodeServerControl(t, client.nextWrite(t))
	if local && (response.Type != protocol.TypeTunnelRecovered || probe.recovered.Load() != 1) {
		t.Fatalf("Unix recovery = %#v, calls=%d", response, probe.recovered.Load())
	}
	if !local && (response.Type != protocol.TypeError || probe.recovered.Load() != 0) {
		t.Fatalf("tailnet recovery = %#v, calls=%d", response, probe.recovered.Load())
	}
	client.pushReadError(io.EOF)
	if err := waitServerResult(t, done, "tunnel control"); err != nil {
		t.Fatal(err)
	}
}

func TestTunnelIngressRejectsOversizedOriginalFrame(t *testing.T) {
	probe := &tunnelRecoveryProbe{}
	server, err := newClientServer(mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector()), failingServerTestConnector(), probe, noServiceControl{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	client := newServerTestConn()
	done := make(chan error, 1)
	go func() { done <- server.Handle(context.Background(), client) }()
	// Unknown JSON fields and insignificant whitespace must still count at ingress.
	payload := `{"type":"tunnel.claim","requestId":"large","unknown":"` + strings.Repeat("x", tunnel.MaximumFrameBytes) + `"}`
	client.pushRead(protocol.Frame{Kind: protocol.KindControl, Payload: []byte(payload)})
	response := decodeServerControl(t, client.nextWrite(t))
	if response.Type != protocol.TypeError || probe.claims.Load() != 0 {
		t.Fatalf("oversize response = %#v, calls=%d", response, probe.claims.Load())
	}
	client.pushReadError(io.EOF)
	if err := waitServerResult(t, done, "tunnel frame cap"); err != nil {
		t.Fatal(err)
	}
}
