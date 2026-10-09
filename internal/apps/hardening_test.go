package apps

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/webauth"
)

func TestAppIDsRejectServeReservedLabels(t *testing.T) {
	for _, id := range []string{"mesh", "apps"} {
		if ValidID(id) {
			t.Errorf("reserved label %q accepted as app ID", id)
		}
	}
	if !ValidID("7k3d") {
		t.Fatal("ordinary app ID rejected")
	}
	if serve.ValidateDeploymentHost("mesh."+Domain()) == nil {
		t.Fatal("private namespace no longer reserved")
	}
}

func TestUnavailableAppHostsHaveUniformResponses(t *testing.T) {
	for _, mode := range []string{"navigation", "cross-site POST", "invalid ticket"} {
		t.Run(mode, func(t *testing.T) {
			f := newAppFixture(t)
			app := createStaticApp(t, f)
			var baseline *httptest.ResponseRecorder
			for _, state := range []string{"private", "expired", "retired", "unknown"} {
				switch state {
				case "expired":
					f.edge.mu.Lock()
					a := f.edge.state.Apps[app.ID]
					a.Status = "expired"
					f.edge.state.Apps[app.ID] = a
					f.edge.publishRuntime(f.edge.state, nil)
					f.edge.mu.Unlock()
				case "retired":
					f.edge.mu.Lock()
					delete(f.edge.state.Apps, app.ID)
					f.edge.publishRuntime(f.edge.state, nil)
					f.edge.mu.Unlock()
				case "unknown":
					f.edgeStore.mu.Lock()
					delete(f.edgeStore.names, app.ID+"."+Domain())
					f.edgeStore.mu.Unlock()
				}
				method := http.MethodGet
				if mode == "cross-site POST" {
					method = http.MethodPost
				}
				target := URL(app.ID) + "/"
				if mode == "invalid ticket" {
					target += "?mesh_view=invalid"
				}
				r := httptest.NewRequest(method, target, nil)
				if mode == "cross-site POST" {
					r.Header.Set("Origin", "https://attacker.example")
					r.Header.Set("Sec-Fetch-Site", "cross-site")
				}
				w := httptest.NewRecorder()
				if !f.edge.ServeHost(w, r, app.ID+"."+Domain()) {
					t.Errorf("%s fell through", state)
					continue
				}
				if baseline == nil {
					baseline = w
					continue
				}
				if w.Code != baseline.Code || w.Body.String() != baseline.Body.String() {
					t.Errorf("%s differs from private refusal: status=%d body=%q", state, w.Code, w.Body.String())
				}
				for _, header := range []string{"Cache-Control", "Referrer-Policy", "Content-Security-Policy", "Cross-Origin-Resource-Policy", "X-Frame-Options"} {
					if w.Header().Get(header) != baseline.Header().Get(header) {
						t.Errorf("%s differs in %s", state, header)
					}
				}
			}
		})
	}
}

func TestNetworkCSRFTokensUseSeparateKeyAndExpire(t *testing.T) {
	f := newAppFixture(t)
	owner := identityFor(f.ownerKey)
	ip := netip.MustParseAddr("100.64.0.2")
	f.edge.config.ClientIP = func(*http.Request) netip.Addr { return ip }
	f.edge.config.NetworkOwners = func(context.Context, netip.Addr) ([]string, error) { return []string{owner}, nil }
	token := func() string {
		r := f.edge.authenticateNetwork(httptest.NewRequest(http.MethodGet, ManagementOrigin()+"/", nil))
		s, err := f.edge.browser(r)
		if err != nil {
			t.Fatal(err)
		}
		return s.CSRF
	}
	old := token()
	raw := hmac.New(sha256.New, f.edgeKey.Seed())
	_, _ = raw.Write([]byte("mesh-app/tailnet-csrf/v1\x00" + ip.String()))
	if old == base64.RawURLEncoding.EncodeToString(raw.Sum(nil)) {
		t.Error("CSRF still uses raw identity seed")
	}
	f.now = f.now.Add(2 * time.Hour)
	fresh := token()
	if fresh == old {
		t.Error("CSRF token did not expire across time buckets")
	}
	mutation := httptest.NewRequest(http.MethodPost, ManagementOrigin()+"/action", nil)
	mutation.Header.Set("Origin", ManagementOrigin())
	mutation.Header.Set("X-Mesh-CSRF", old)
	session, err := f.edge.browser(f.edge.authenticateNetwork(mutation))
	if err != nil {
		t.Fatal(err)
	}
	if webauth.ValidateSessionMutation(mutation, ManagementOrigin(), session) == nil {
		t.Fatal("expired network CSRF authorized a mutation")
	}
	mutation.Header.Set("X-Mesh-CSRF", fresh)
	if err := webauth.ValidateSessionMutation(mutation, ManagementOrigin(), session); err != nil {
		t.Fatalf("fresh network CSRF rejected: %v", err)
	}
}

func TestDownloadRefusesCrossSiteTopLevelNavigation(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	owner := pairedOwner(t, f)
	r := httptest.NewRequest(http.MethodGet, ManagementOrigin()+"/download?id="+app.ID, nil)
	r.AddCookie(owner)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "document")
	w := httptest.NewRecorder()
	f.edge.ServeHost(w, r, ManagementHost())
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Owner authorization required") {
		t.Fatalf("cross-site download got %d %q", w.Code, w.Body.String())
	}
}

func TestPrivateViewTicketCannotBeUsedWithoutBrowserNonce(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	owner := pairedOwner(t, f)
	r := httptest.NewRequest(http.MethodGet, ManagementOrigin()+"/view?id="+app.ID, nil)
	r.AddCookie(owner)
	redirect, nonce := privateViewRedirect(t, f, r)
	target := redirect.Header().Get("Location")
	if !strings.Contains(target, "mesh_view=") {
		t.Fatalf("view ticket missing: %s", target)
	}
	consume := httptest.NewRecorder()
	f.edge.ServeHost(consume, httptest.NewRequest(http.MethodGet, target, nil), app.ID+"."+Domain())
	if consume.Code != http.StatusForbidden {
		t.Fatalf("stolen view URL got %d", consume.Code)
	}
	legitimate := httptest.NewRequest(http.MethodGet, target, nil)
	legitimate.AddCookie(nonce)
	accepted := httptest.NewRecorder()
	f.edge.ServeHost(accepted, legitimate, app.ID+"."+Domain())
	if accepted.Code != http.StatusSeeOther {
		t.Fatalf("bound browser could not consume ticket: %d %s", accepted.Code, accepted.Body.String())
	}
}

func privateViewRedirect(t *testing.T, f *appFixture, management *http.Request) (*httptest.ResponseRecorder, *http.Cookie) {
	t.Helper()
	first := httptest.NewRecorder()
	f.edge.ServeHost(first, management, ManagementHost())
	challenge := httptest.NewRequest(http.MethodGet, first.Header().Get("Location"), nil)
	start := httptest.NewRecorder()
	f.edge.ServeHost(start, challenge, challenge.URL.Host)
	nonce := cookieNamed(t, start, webauth.ViewNonceCookie)
	resume := httptest.NewRequest(http.MethodGet, start.Header().Get("Location"), nil)
	for _, cookie := range management.Cookies() {
		resume.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	f.edge.ServeHost(response, resume, ManagementHost())
	return response, nonce
}

func TestOwnerDownloadAllowsIntentionalRequests(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	networkOrigin(t, f)
	owner := pairedOwner(t, f)
	for _, site := range []string{"same-origin", "none", ""} {
		r := httptest.NewRequest(http.MethodGet, ManagementOrigin()+"/download?id="+app.ID, nil)
		r.AddCookie(owner)
		r.Header.Set("Sec-Fetch-Site", site)
		w := httptest.NewRecorder()
		f.edge.ServeHost(w, r, ManagementHost())
		if w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Errorf("intentional download site=%q got %d", site, w.Code)
		}
	}
}
