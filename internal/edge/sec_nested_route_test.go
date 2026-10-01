package edge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/tunnel"
)

func TestSecuritySlowTunnelDialDoesNotWithdrawTunnel(t *testing.T) {
	controller, registry, claimant := newProxyTunnel(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "healthy tunnel")
	}))
	defer origin.Close()
	healthy := proxyEndpointFor(origin)
	var dials atomic.Int32
	endpoint := &proxyTunnelEndpoint{dial: func(ctx context.Context) (net.Conn, error) {
		if dials.Add(1) == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return healthy.Dial(ctx)
	}}
	activateProxyTunnel(t, controller, claimant, endpoint)
	route := registry.findTunnel(proxyTunnelName)
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, publicRequest(http.MethodGet, proxyTunnelName, "/"))
	if registry.findTunnel(proxyTunnelName) != route || endpoint.closed.Load() != 0 {
		t.Fatalf("one slow anonymous request withdrew the reverse tunnel (response %d)", recorder.Code)
	}
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("slow dial response = %d, want 504", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	registry.ServeHTTP(recorder, publicRequest(http.MethodGet, proxyTunnelName, "/"))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "healthy tunnel" {
		t.Fatalf("next request = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestTunnelTransientDialErrorsKeepActivation(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, tunnel.ErrCapacity, io.EOF,
		errors.New("channel rejected"), fmt.Errorf("wrapped cancellation: %w", context.Canceled)} {
		t.Run(err.Error(), func(t *testing.T) {
			controller, registry, claimant := newProxyTunnel(t)
			endpoint := &proxyTunnelEndpoint{dial: func(context.Context) (net.Conn, error) { return nil, err }}
			activateProxyTunnel(t, controller, claimant, endpoint)
			route := registry.findTunnel(proxyTunnelName)
			if _, got := route.dial(context.Background(), "tcp", proxyTunnelName); !errors.Is(got, err) {
				t.Fatalf("Dial error = %v, want %v", got, err)
			}
			if registry.findTunnel(proxyTunnelName) != route || endpoint.closed.Load() != 0 {
				t.Fatal("request-level dial failure deactivated the tunnel")
			}
		})
	}
}
