package apps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/webauth"
)

func ambientOwnerRequest(t *testing.T, f *appFixture, app Record, credential string) func(*http.Request) {
	t.Helper()
	if credential == "network" {
		ownerIP := netip.MustParseAddr("100.64.0.2")
		f.edge.config.ClientIP = func(r *http.Request) netip.Addr {
			address, _ := netip.ParseAddrPort(r.RemoteAddr)
			return address.Addr()
		}
		f.edge.config.NetworkOwners = func(_ context.Context, ip netip.Addr) ([]string, error) {
			if ip == ownerIP {
				return []string{app.Owner}, nil
			}
			return nil, nil
		}
		return func(r *http.Request) { r.RemoteAddr = ownerIP.String() + ":4444" }
	}
	owner := pairedOwner(t, f)
	request := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/view?id="+app.ID, nil)
	request.AddCookie(owner)
	redirect := httptest.NewRecorder()
	f.edge.ServeHost(redirect, request, ManagementHost)
	consume := httptest.NewRecorder()
	f.edge.ServeHost(consume, httptest.NewRequest(http.MethodGet, redirect.Header().Get("Location"), nil), app.ID+"."+Domain)
	view := cookieNamed(t, consume, webauth.ViewCookie)
	return func(r *http.Request) { r.AddCookie(view) }
}

func TestPrivateAppRejectsAmbientOwnerFromOtherPages(t *testing.T) {
	for _, credential := range []string{"network", "view cookie"} {
		for _, tc := range []struct {
			name, method, site, mode, dest, origin, upgrade string
		}{
			{name: "cross-site subresource", method: http.MethodGet, site: "cross-site", mode: "no-cors", dest: "image", origin: "https://attacker.example"},
			{name: "same-site subresource", method: http.MethodGet, site: "same-site", mode: "cors", dest: "empty", origin: "https://zzzz.shaulavo.dev"},
			{name: "cross-site iframe", method: http.MethodGet, site: "cross-site", mode: "navigate", dest: "iframe"},
			{name: "same-site iframe", method: http.MethodGet, site: "same-site", mode: "navigate", dest: "iframe"},
			{name: "cross-site form POST", method: http.MethodPost, site: "cross-site", mode: "navigate", dest: "document", origin: "https://attacker.example"},
			{name: "same-site POST", method: http.MethodPost, site: "same-site", mode: "cors", dest: "empty", origin: "https://zzzz.shaulavo.dev"},
			{name: "legacy cross-origin POST", method: http.MethodPost, origin: "https://attacker.example"},
			{name: "cross-origin websocket", method: http.MethodGet, site: "same-origin", origin: "https://attacker.example", upgrade: "websocket"},
			{name: "websocket missing origin", method: http.MethodGet, site: "same-origin", upgrade: "websocket"},
		} {
			t.Run(credential+"/"+tc.name, func(t *testing.T) {
				f := newAppFixture(t)
				app := createStaticApp(t, f)
				forwarded := networkOrigin(t, f)
				authenticate := ambientOwnerRequest(t, f, app, credential)
				r := httptest.NewRequest(tc.method, URL(app.ID)+"/index.html", strings.NewReader("confirm=yes"))
				authenticate(r)
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.Header.Set("Sec-Fetch-Site", tc.site)
				r.Header.Set("Sec-Fetch-Mode", tc.mode)
				r.Header.Set("Sec-Fetch-Dest", tc.dest)
				r.Header.Set("Origin", tc.origin)
				if tc.upgrade != "" {
					r.Header.Set("Connection", "Upgrade")
					r.Header.Set("Upgrade", tc.upgrade)
				}
				w := httptest.NewRecorder()
				f.edge.ServeHost(w, r, app.ID+"."+Domain)
				if w.Code != http.StatusForbidden || forwarded.Load() != 0 {
					t.Fatalf("private request status=%d, upstream hits=%d; want 403 and zero", w.Code, forwarded.Load())
				}
				current, _, err := f.edge.lookup(context.Background(), app.ID, false)
				if err != nil || !current.ExpiresAt.Equal(app.ExpiresAt) {
					t.Fatalf("rejected request changed app deadline: %v", err)
				}
			})
		}
	}
}

func TestPrivateAppAdmissionRejectsAmbientOwnerFromOtherPages(t *testing.T) {
	for _, credential := range []string{"network", "view cookie"} {
		t.Run(credential, func(t *testing.T) {
			f := newAppFixture(t)
			app := createStaticApp(t, f)
			authenticate := ambientOwnerRequest(t, f, app, credential)
			r := httptest.NewRequest(http.MethodPost, URL(app.ID)+"/api/delete-everything", strings.NewReader("confirm=yes"))
			authenticate(r)
			r.Header.Set("Sec-Fetch-Site", "same-site")
			r.Header.Set("Origin", "https://zzzz.shaulavo.dev")
			r = f.edge.authenticateNetwork(r)
			_, _, release, err := f.edge.admit(r, app.ID)
			if release != nil {
				release()
			}
			if err == nil {
				t.Fatal("admission granted ambient owner authority to sibling page")
			}
		})
	}
}
