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
	if serve.ValidatePublicName("mesh."+Domain) == nil {
		t.Fatal("private namespace no longer reserved")
	}
}

func TestUnavailableAppHostsHaveUniformResponses(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		t.Run(map[bool]string{false: "navigation", true: "cross-site POST"}[unsafe], func(t *testing.T) {
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
					delete(f.edgeStore.names, app.ID+"."+Domain)
					f.edgeStore.mu.Unlock()
				}
				method := http.MethodGet
				if unsafe {
					method = http.MethodPost
				}
				r := httptest.NewRequest(method, URL(app.ID)+"/", nil)
				if unsafe {
					r.Header.Set("Origin", "https://attacker.example")
					r.Header.Set("Sec-Fetch-Site", "cross-site")
				}
				w := httptest.NewRecorder()
				if !f.edge.ServeHost(w, r, app.ID+"."+Domain) {
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
		r := f.edge.authenticateNetwork(httptest.NewRequest(http.MethodGet, ManagementOrigin+"/", nil))
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
	if token() == old {
		t.Error("CSRF token did not expire across time buckets")
	}
}

func TestDownloadRefusesCrossSiteTopLevelNavigation(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	owner := pairedOwner(t, f)
	r := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/download?id="+app.ID, nil)
	r.AddCookie(owner)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "document")
	w := httptest.NewRecorder()
	f.edge.ServeHost(w, r, ManagementHost)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Owner authorization required") {
		t.Fatalf("cross-site download got %d %q", w.Code, w.Body.String())
	}
}

func TestPrivateViewTicketCannotBeUsedWithoutBrowserNonce(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	owner := pairedOwner(t, f)
	r := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/view?id="+app.ID, nil)
	r.AddCookie(owner)
	w := httptest.NewRecorder()
	f.edge.ServeHost(w, r, ManagementHost)
	target := w.Header().Get("Location")
	if strings.Contains(target, "mesh_view=") {
		consume := httptest.NewRecorder()
		f.edge.ServeHost(consume, httptest.NewRequest(http.MethodGet, target, nil), app.ID+"."+Domain)
		if consume.Code == http.StatusSeeOther {
			t.Fatal("stolen view URL granted a private session without a browser nonce")
		}
	}
}
