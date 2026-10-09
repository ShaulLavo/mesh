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
		tls             bool
		owners          []string
		err             error
		code            int
	}{
		{name: "owner", peer: "100.64.0.9:4000", sni: "7k3d.mesh.test", tls: true, owners: []string{"owner"}, code: 204},
		{name: "owner IPv6", peer: "[fd7a:115c:a1e0::9]:4000", sni: "7k3d.mesh.test", tls: true, owners: []string{"owner"}, code: 204},
		{name: "public with cookies", peer: "203.0.113.9:4000", sni: "7k3d.mesh.test", tls: true, owners: []string{"owner"}, code: 404},
		{name: "loopback with cookies", peer: "127.0.0.1:4000", sni: "7k3d.mesh.test", tls: true, owners: []string{"owner"}, code: 404},
		{name: "plaintext", peer: "100.64.0.9:4000", owners: []string{"owner"}, code: 404},
		{name: "unknown tailnet user", peer: "100.64.0.9:4000", sni: "7k3d.mesh.test", tls: true, code: 404},
		{name: "resolver unavailable", peer: "100.64.0.9:4000", sni: "7k3d.mesh.test", tls: true, err: errors.New("offline"), code: 404},
		{name: "coalesced TLS host", peer: "100.64.0.9:4000", sni: "other.mesh.test", tls: true, owners: []string{"owner"}, code: 421},
		{name: "missing SNI", peer: "100.64.0.9:4000", tls: true, owners: []string{"owner"}, code: 421},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkPrivateAppIngress(t, tc.name, tc.peer, tc.sni, tc.tls, tc.owners, tc.err, tc.code)
		})
	}
}

func checkPrivateAppIngress(t *testing.T, name, peer, sni string, secure bool, owners []string, resolveErr error, code int) {
	t.Helper()
	app := &privateAppFixture{}
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
	privateAppsHTTPHandler(app, resolve).ServeHTTP(response, request)
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

func TestPrivateAppDispatcherRejectsUnknownHosts(t *testing.T) {
	app := &privateAppFixture{}
	response := httptest.NewRecorder()
	privateAppsHTTPHandler(app, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://docs.mesh.test/docs/", nil))
	if response.Code != http.StatusNotFound || app.calls != 0 {
		t.Fatalf("unknown host dispatch: %d", response.Code)
	}
}
