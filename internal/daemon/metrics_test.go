package daemon

import (
	"context"
	"io"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
)

func TestClientServerHostMetricsRPCIsSharedAndCorrelated(t *testing.T) {
	client := newServerTestConn()
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector(), disabledEdgeController{}, noServiceControl{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Handle(context.Background(), client) }()
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeHostInfo, RequestID: "identity"}))
	identity := decodeServerControl(t, client.nextWrite(t))
	if identity.Host == nil || identity.Host.ID != "host-a" {
		t.Fatalf("identity = %+v", identity)
	}
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeHostMetrics, RequestID: "metrics-1"}))
	first := decodeServerControl(t, client.nextWrite(t))
	if first.Type != protocol.TypeHostMetricsResult || first.RequestID != "metrics-1" || first.Metrics == nil {
		t.Fatalf("metrics response = %+v", first)
	}
	if err := protocol.ValidateHostMetrics(*first.Metrics); err != nil {
		t.Fatal(err)
	}
	if first.Metrics.Memory.State != protocol.MetricAvailable || first.Metrics.Uptime.State != protocol.MetricAvailable {
		t.Fatalf("host metrics unavailable: %+v", first.Metrics)
	}
	if first.Metrics.CPU.State != protocol.MetricUnavailable || first.Metrics.CPU.Sample != "" {
		t.Fatalf("cold CPU had no baseline: %+v", first.Metrics.CPU)
	}
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeHostMetrics, RequestID: "metrics-2"}))
	second := decodeServerControl(t, client.nextWrite(t))
	if second.Metrics == nil || second.Metrics.Memory.Sample != first.Metrics.Memory.Sample || second.RequestID != "metrics-2" {
		t.Fatalf("second viewer did not share cache: %+v", second)
	}
	client.pushReadError(io.EOF)
	if err := waitServerResult(t, done, "metrics server"); err != nil {
		t.Fatal(err)
	}
}
