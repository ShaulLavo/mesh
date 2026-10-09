package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

type privateAppFixture struct{ calls int }

func (f *privateAppFixture) HasHost(host string) bool {
	return host == "7k3d.mesh.test" || host == "apps.mesh.test"
}
func (f *privateAppFixture) ServeHost(w http.ResponseWriter, _ *http.Request, _ string) bool {
	f.calls++
	w.WriteHeader(http.StatusNoContent)
	return true
}

func TestPrivateAppIngressRequiresVerifiedTailnetTLS(t *testing.T) {
	cases := []struct {
		name, peer, sni string
		tls, enabled    bool
		owners          []string
		err             error
		code            int
	}{
		{name: "owner", peer: "100.64.0.9:4000", sni: "7k3d.mesh.test", tls: true, enabled: true, owners: []string{"owner"}, code: 204},
		{name: "owner IPv6", peer: "[fd7a:115c:a1e0::9]:4000", sni: "7k3d.mesh.test", tls: true, enabled: true, owners: []string{"owner"}, code: 204},
		{name: "public with cookies", peer: "203.0.113.9:4000", sni: "7k3d.mesh.test", tls: true, enabled: true, owners: []string{"owner"}, code: 404},
		{name: "loopback with cookies", peer: "127.0.0.1:4000", sni: "7k3d.mesh.test", tls: true, enabled: true, owners: []string{"owner"}, code: 404},
		{name: "plaintext", peer: "100.64.0.9:4000", enabled: true, owners: []string{"owner"}, code: 404},
		{name: "disabled trusted forwarding", peer: "100.64.0.9:4000", sni: "7k3d.mesh.test", tls: true, owners: []string{"owner"}, code: 404},
		{name: "unknown tailnet user", peer: "100.64.0.9:4000", sni: "7k3d.mesh.test", tls: true, enabled: true, code: 404},
		{name: "resolver unavailable", peer: "100.64.0.9:4000", sni: "7k3d.mesh.test", tls: true, enabled: true, err: errors.New("offline"), code: 404},
		{name: "coalesced TLS host", peer: "100.64.0.9:4000", sni: "other.mesh.test", tls: true, enabled: true, owners: []string{"owner"}, code: 421},
		{name: "missing SNI", peer: "100.64.0.9:4000", tls: true, enabled: true, owners: []string{"owner"}, code: 421},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkPrivateAppIngress(t, tc.name, tc.peer, tc.sni, tc.tls, tc.enabled, tc.owners, tc.err, tc.code)
		})
	}
}

func checkPrivateAppIngress(t *testing.T, name, peer, sni string, secure, enabled bool, owners []string, resolveErr error, code int) {
	t.Helper()
	app := &privateAppFixture{}
	fallback := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("app escaped into ordinary public routes") })
	resolve := func(_ context.Context, ip netip.Addr) ([]string, error) {
		if ip != netip.MustParseAddrPort(peer).Addr().Unmap() {
			t.Fatalf("owner lookup trusted forwarded headers: %s", ip)
		}
		return owners, resolveErr
	}
	request := httptest.NewRequest(http.MethodGet, "https://7k3d.mesh.test/", nil)
	request.RemoteAddr = peer
	request.TLS = nil
	if secure {
		request.TLS = &tls.ConnectionState{ServerName: sni}
	}
	request.Header.Set("X-Forwarded-For", "100.64.0.9")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.AddCookie(&http.Cookie{Name: "__Host-mesh-owner", Value: "paired", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	response := httptest.NewRecorder()
	privateAppsHTTPHandler(app, fallback, resolve, enabled).ServeHTTP(response, request)
	if response.Code != code {
		t.Fatalf("%s status=%d, want %d", name, response.Code, code)
	}
	if (code == http.StatusNoContent) != (app.calls == 1) {
		t.Fatalf("%s private dispatch calls=%d", name, app.calls)
	}
	if code == http.StatusMisdirectedRequest && (response.Body.Len() != 0 || response.Header().Get("Content-Type") != "") {
		t.Fatal("coalesced rejection has renderable content")
	}
}

func TestPrivateAppDispatcherPreservesOrdinaryRoutes(t *testing.T) {
	app := &privateAppFixture{}
	fallback := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	response := httptest.NewRecorder()
	privateAppsHTTPHandler(app, fallback, nil, false).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://docs.mesh.test/docs/", nil))
	if response.Code != http.StatusAccepted || app.calls != 0 {
		t.Fatalf("ordinary route changed: %d", response.Code)
	}
}
