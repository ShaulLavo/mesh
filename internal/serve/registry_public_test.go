package serve

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
)

func TestRegistryPublicHostRejectsNestedPrivateRoutes(t *testing.T) {
	for _, demand := range []bool{false, true} {
		for _, peer := range []struct {
			name   string
			host   string
			remote string
		}{
			{name: "direct tailnet peer", host: "app.shaulavo.dev", remote: "100.64.0.3:40000"},
			{name: "public host with port", host: "app.shaulavo.dev:443", remote: "100.64.0.3:40000"},
			{name: "public host uppercase", host: "APP.SHAULAVO.DEV", remote: "100.64.0.2:40000"},
			{name: "public host root dot", host: "app.shaulavo.dev.", remote: "100.64.0.2:40000"},
			{name: "public host case dot and port", host: "APP.SHAULAVO.DEV.:443", remote: "100.64.0.2:40000"},
			{name: "pinned edge", host: "app.shaulavo.dev", remote: "100.64.0.2:40000"},
		} {
			name := "proxy/" + peer.name
			if demand {
				name = "on-demand/" + peer.name
			}
			t.Run(name, func(t *testing.T) {
				var hits atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					hits.Add(1)
					w.WriteHeader(http.StatusNoContent)
				}))
				defer upstream.Close()
				_, port, err := net.SplitHostPort(upstream.Listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				nested := Service{Name: "app/admin", Kind: Proxy, Target: port}
				if demand {
					nested.Demand = &Demand{Command: "test-command", Cwd: t.TempDir()}
				}
				registry, err := NewRegistryWithReservedPrefix([]Service{
					{Name: "app", Kind: Proxy, Target: port, PublicName: "app.shaulavo.dev"},
					nested,
				}, ReservedPrefix, func(address netip.Addr) bool { return address == netip.MustParseAddr("100.64.0.2") })
				if err != nil {
					t.Fatal(err)
				}
				gate := &fakeGate{err: errString("unexpected on-demand start")}
				registry.SetDemandGate(gate, nil)
				request := httptest.NewRequest(http.MethodGet, "/app/admin/secrets", nil)
				request.Host = peer.host
				request.RemoteAddr = peer.remote
				response := httptest.NewRecorder()
				registry.ServeHTTP(response, request)
				if response.Code != http.StatusNotFound || hits.Load() != 0 || len(gate.entered) != 0 {
					t.Fatalf("public request reached nested private route: status=%d upstream hits=%d demand starts=%v", response.Code, hits.Load(), gate.entered)
				}
			})
		}
	}
}

func TestRegistryPublicHostRouting(t *testing.T) {
	for _, test := range []struct {
		name             string
		host             string
		path             string
		remote           string
		nestedPublicName string
		wantStatus       int
		wantHits         int32
		wantPrefix       string
	}{
		{name: "public parent", host: "app.shaulavo.dev", path: "/app/value", wantStatus: http.StatusNoContent, wantHits: 1, wantPrefix: "/app"},
		{name: "public parent case dot and port", host: "APP.SHAULAVO.DEV.:443", path: "/app/value", wantStatus: http.StatusNoContent, wantHits: 1, wantPrefix: "/app"},
		{name: "unknown public host case and dot", host: "OTHER.SHAULAVO.DEV.:443", path: "/app/value", wantStatus: http.StatusNotFound},
		{name: "public bare mount", host: "app.shaulavo.dev", path: "/app", wantStatus: http.StatusPermanentRedirect},
		{name: "public mount boundary", host: "app.shaulavo.dev", path: "/application/value", wantStatus: http.StatusNotFound},
		{name: "unknown public host", host: "other.shaulavo.dev", path: "/app/value", wantStatus: http.StatusNotFound},
		{name: "nested public same host", host: "app.shaulavo.dev", path: "/app/admin/value", nestedPublicName: "app.shaulavo.dev", wantStatus: http.StatusNoContent, wantHits: 1, wantPrefix: "/app/admin"},
		{name: "nested public other host", host: "app.shaulavo.dev", path: "/app/admin/value", nestedPublicName: "admin.shaulavo.dev", wantStatus: http.StatusNotFound},
		{name: "nested public own host", host: "admin.shaulavo.dev", path: "/app/admin/value", nestedPublicName: "admin.shaulavo.dev", wantStatus: http.StatusNoContent, wantHits: 1, wantPrefix: "/app/admin"},
		{name: "tailnet private name", host: "pc.mesh.shaulavo.dev", path: "/app/admin/value", wantStatus: http.StatusNoContent, wantHits: 1, wantPrefix: "/app/admin"},
		{name: "tailnet private namespace", host: "mesh.shaulavo.dev", path: "/app/admin/value", wantStatus: http.StatusNoContent, wantHits: 1, wantPrefix: "/app/admin"},
		{name: "tailnet IP", host: "100.64.0.1:7337", path: "/app/admin/value", wantStatus: http.StatusNoContent, wantHits: 1, wantPrefix: "/app/admin"},
		{name: "tailnet IPv6", host: "[fd7a:115c:a1e0::1]:7337", path: "/app/admin/value", wantStatus: http.StatusNoContent, wantHits: 1, wantPrefix: "/app/admin"},
		{name: "pinned edge private host", host: "pc.mesh.shaulavo.dev", path: "/app/admin/value", remote: "100.64.0.2:40000", wantStatus: http.StatusNoContent, wantHits: 1, wantPrefix: "/app/admin"},
		{name: "pinned edge IP host", host: "100.64.0.1:7337", path: "/app/admin/value", remote: "100.64.0.2:40000", wantStatus: http.StatusNoContent, wantHits: 1, wantPrefix: "/app/admin"},
		{name: "pinned mapped edge", host: "pc.mesh.shaulavo.dev", path: "/app/admin/value", remote: "[::ffff:100.64.0.2]:40000", wantStatus: http.StatusNoContent, wantHits: 1, wantPrefix: "/app/admin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var hits atomic.Int32
			prefixes := make(chan string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				prefixes <- r.Header.Get("X-Forwarded-Prefix")
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			_, port, err := net.SplitHostPort(upstream.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			registry, err := NewRegistryWithReservedPrefix([]Service{
				{Name: "app", Kind: Proxy, Target: port, PublicName: "app.shaulavo.dev"},
				{Name: "app/admin", Kind: Proxy, Target: port, PublicName: test.nestedPublicName},
			}, ReservedPrefix, func(address netip.Addr) bool { return address == netip.MustParseAddr("100.64.0.2") })
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request.Host = test.host
			request.RemoteAddr = "100.64.0.3:40000"
			if test.remote != "" {
				request.RemoteAddr = test.remote
			}
			request.Header.Set("X-Forwarded-Host", "pc.mesh.shaulavo.dev")
			request.Header.Set("X-Forwarded-For", "203.0.113.50")
			request.Header.Set("X-Forwarded-Proto", "https")
			response := httptest.NewRecorder()
			registry.ServeHTTP(response, request)
			if response.Code != test.wantStatus || hits.Load() != test.wantHits {
				t.Fatalf("response=%d upstream hits=%d, want response=%d upstream hits=%d", response.Code, hits.Load(), test.wantStatus, test.wantHits)
			}
			if test.wantHits != 0 {
				if got := <-prefixes; got != test.wantPrefix {
					t.Fatalf("selected route=%q, want %q", got, test.wantPrefix)
				}
			}
		})
	}
}
