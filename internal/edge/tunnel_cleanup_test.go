package edge

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestTunnelCleanupMarksDeadBeforeMutationGate(t *testing.T) {
	c, registry, claimant := newProxyTunnel(t)
	var dials atomic.Int32
	endpoint := &proxyTunnelEndpoint{dial: func(context.Context) (net.Conn, error) { dials.Add(1); return nil, net.ErrClosed }}
	release, err := c.ActivateTunnel(context.Background(), claimant, proxyTunnelName, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.acquireCommit(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { release(); close(done) }()
	route := registry.findTunnel(proxyTunnelName)
	deadline := time.Now().Add(time.Second)
	for route.active.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	inactive := !route.active.Load()
	status := 0
	if inactive {
		response := httptest.NewRecorder()
		registry.ServeHTTP(response, publicRequest(http.MethodGet, proxyTunnelName, "/"))
		status = response.Code
	}
	select {
	case <-done:
		c.releaseCommit()
		t.Fatal("cleanup bypassed the mutation gate")
	default:
	}
	c.releaseCommit()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish after mutation gate released")
	}
	if !inactive || status != http.StatusNotFound || dials.Load() != 0 || registry.findTunnel(proxyTunnelName) != nil {
		t.Fatal("dead token remained available during durable mutation")
	}
}
