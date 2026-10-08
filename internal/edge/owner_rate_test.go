package edge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestOwnerRequestsHaveNoMinuteQuota(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	now := time.Now()
	ownerAddress := netip.MustParseAddr("100.64.0.2")
	registry, err := NewRegistry(HandlerConfig{
		Mode: ModeDirectTLS, Now: func() time.Time { return now },
		RateLimitExempt: func(_ context.Context, address netip.Addr) bool { return address == ownerAddress },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	origin, _ := testIdentity(t)
	if err := registry.Replace([]PublishedRoute{{
		Route:  Route{PublicName: "app.mesh.test", ServiceName: "assets"},
		Origin: testResolvedOrigin(origin, testHTTPServerEndpoint(t, backend), now),
	}}); err != nil {
		t.Fatal(err)
	}
	for i := range 3 * maximumRequestsPerMinute {
		request := publicRequest(http.MethodGet, "app.mesh.test", "/assets/image.png")
		request.RemoteAddr = netip.AddrPortFrom(ownerAddress, 12345).String()
		response := httptest.NewRecorder()
		registry.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("owner request %d: status %d, body %q", i+1, response.Code, response.Body.String())
		}
	}
	if len(registry.rate.entries) != 0 {
		t.Fatal("owner traffic consumed the anonymous request quota")
	}
	for i := 0; i <= maximumRequestsPerMinute; i++ {
		request := publicRequest(http.MethodGet, "app.mesh.test", "/assets/image.png")
		request.Header.Set("X-Forwarded-For", ownerAddress.String())
		response := httptest.NewRecorder()
		registry.ServeHTTP(response, request)
		want := http.StatusNoContent
		if i == maximumRequestsPerMinute {
			want = http.StatusTooManyRequests
		}
		if response.Code != want {
			t.Fatalf("anonymous request %d with forged owner header: status %d, want %d", i+1, response.Code, want)
		}
	}
}

func TestOwnerExemptionUsesVerifiedForwardedAddress(t *testing.T) {
	ownerAddress := netip.MustParseAddr("100.64.0.2")
	registry, err := NewRegistry(HandlerConfig{
		Mode:            ModeProxy,
		RateLimitExempt: func(_ context.Context, address netip.Addr) bool { return address == ownerAddress },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	registry.forwarderTrusted = func(*http.Request) bool { return true }
	for range 2 * maximumRequestsPerMinute {
		request := publicRequest(http.MethodGet, "app.mesh.test", "/")
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set("X-Forwarded-For", ownerAddress.String())
		request.Header.Set("X-Forwarded-Proto", "https")
		response := httptest.NewRecorder()
		registry.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("verified owner: status %d, want 404", response.Code)
		}
	}
	registry.forwarderTrusted = func(*http.Request) bool { return false }
	for i := 0; i <= maximumRequestsPerMinute; i++ {
		request := publicRequest(http.MethodGet, "app.mesh.test", "/")
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set("X-Forwarded-For", ownerAddress.String())
		request.Header.Set("X-Forwarded-Proto", "https")
		response := httptest.NewRecorder()
		registry.ServeHTTP(response, request)
		want := http.StatusNotFound
		if i == maximumRequestsPerMinute {
			want = http.StatusTooManyRequests
		}
		if response.Code != want {
			t.Fatalf("unverified forwarder request %d: status %d, want %d", i+1, response.Code, want)
		}
	}
}
