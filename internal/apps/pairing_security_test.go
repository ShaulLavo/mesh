package apps

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/webauth"
)

type countingPairStore struct {
	*memoryAppStore
	writes int
}

func (s *countingPairStore) SaveAppState(ctx context.Context, key string, raw []byte) error {
	s.writes++
	return s.memoryAppStore.SaveAppState(ctx, key, raw)
}

func pairingRequest(method, path, source string, cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequest(method, ManagementOrigin()+path, nil)
	r.RemoteAddr = source + ":1234"
	r.Header.Set("Origin", ManagementOrigin())
	r.Header.Set("User-Agent", "Mozilla/5.0 TestBrowser/1.0")
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	return r
}

func TestAnonymousGETFloodLeavesPairingAvailableWithoutWrites(t *testing.T) {
	f := newAppFixture(t)
	store := &countingPairStore{memoryAppStore: f.edgeStore}
	config := f.edge.config
	config.Store = store
	var err error
	f.edge, err = NewEdge(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	store.writes = 0
	for range 256 {
		response := httptest.NewRecorder()
		f.edge.ServeHost(response, pairingRequest(http.MethodGet, "/", "192.0.2.1"), ManagementHost())
		if response.Code != http.StatusOK {
			t.Fatalf("anonymous GET returned %d", response.Code)
		}
	}
	for _, path := range []string{"/pair", "/view?id=7k3d", "/confirm?id=7k3d&action=public"} {
		response := httptest.NewRecorder()
		f.edge.ServeHost(response, pairingRequest(http.MethodGet, path, "192.0.2.1"), ManagementHost())
		if len(response.Result().Cookies()) != 0 {
			t.Fatalf("GET %s allocated a pair cookie", path)
		}
	}
	if store.writes != 0 {
		t.Fatalf("anonymous GETs made %d durable writes", store.writes)
	}
	legitimate := httptest.NewRecorder()
	f.edge.ServeHost(legitimate, pairingRequest(http.MethodPost, "/pair", "192.0.2.2"), ManagementHost())
	if legitimate.Code != http.StatusOK {
		t.Fatalf("legitimate pairing after 256 anonymous GETs = %d, want 200", legitimate.Code)
	}
	cookieNamed(t, legitimate, webauth.PairCookie)
	if store.writes != 0 {
		t.Fatalf("unapproved pairing made %d durable writes", store.writes)
	}
}

func TestExplicitPairingPreservesCodeOnGETAndChecksOrigin(t *testing.T) {
	f := newAppFixture(t)
	begin := httptest.NewRecorder()
	f.edge.ServeHost(begin, pairingRequest(http.MethodPost, "/pair", "192.0.2.1"), ManagementHost())
	if begin.Code != http.StatusOK {
		t.Fatalf("explicit pairing = %d, want 200", begin.Code)
	}
	codePattern := regexp.MustCompile(`Code: <strong>([a-z0-9-]+)</strong>`)
	code := codePattern.FindStringSubmatch(begin.Body.String())
	if len(code) != 2 {
		t.Fatal("no explicit pairing code")
	}
	for range 10 {
		r := pairingRequest(http.MethodGet, "/pair", "192.0.2.1", cookieNamed(t, begin, webauth.PairCookie))
		response := httptest.NewRecorder()
		f.edge.ServeHost(response, r, ManagementHost())
		got := codePattern.FindStringSubmatch(response.Body.String())
		if len(got) != 2 || got[1] != code[1] {
			t.Fatal("repeated GET changed pending pair")
		}
	}
	for _, origin := range []string{"", "https://evil.example", "https://7k3d.mesh.test"} {
		r := pairingRequest(http.MethodPost, "/pair", "192.0.2.2")
		r.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		f.edge.ServeHost(response, r, ManagementHost())
		if response.Code != http.StatusForbidden || len(response.Result().Cookies()) != 0 {
			t.Fatalf("pairing origin %q = %d", origin, response.Code)
		}
	}
}

func TestPairingInspectionReportsTrustedSourceBrowserAndAge(t *testing.T) {
	f := newAppFixture(t)
	f.edge.config.ClientIP = func(*http.Request) netip.Addr { return netip.MustParseAddr("192.0.2.9") }
	begin := httptest.NewRecorder()
	r := pairingRequest(http.MethodPost, "/pair", "127.0.0.1")
	r.Header.Set("X-Forwarded-For", "203.0.113.66")
	f.edge.ServeHost(begin, r, ManagementHost())
	code := regexp.MustCompile(`Code: <strong>([a-z0-9-]+)</strong>`).FindStringSubmatch(begin.Body.String())
	if len(code) != 2 {
		t.Fatal("missing pending code")
	}
	f.now = f.now.Add(90 * time.Second)
	result, err := f.origin.Handle(context.Background(), Request{Action: "browser.inspect", Code: code[1]})
	if err != nil {
		t.Fatalf("owner cannot inspect browser: %v", err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"TestBrowser/1.0", "192.0.2.9", `"ageSeconds":90`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("inspection missing %s: %s", field, raw)
		}
	}
	if strings.Contains(string(raw), "203.0.113.66") {
		t.Fatal("inspection trusted a visitor-supplied forwarded IP")
	}
}

func TestConfirmationStartsInertAndCannotBeFramed(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	cookie := pairedOwner(t, f)
	r := pairingRequest(http.MethodGet, "/confirm?id="+app.ID+"&action=public", "192.0.2.1", cookie)
	response := httptest.NewRecorder()
	f.edge.ServeHost(response, r, ManagementHost())
	if !strings.Contains(response.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("confirm can be framed")
	}
	if !regexp.MustCompile(`<button[^>]*\bdisabled\b[^>]*>Confirm public</button>`).MatchString(response.Body.String()) {
		t.Fatal("confirm button is active immediately")
	}
	for _, guard := range []string{"document.hasFocus()", "visibilitychange", "pointermove", "keydown", "750"} {
		if !strings.Contains(response.Body.String(), guard) {
			t.Fatalf("confirm missing activation guard %s", guard)
		}
	}
}

func TestPublicAndDeleteRequireServerConfirmation(t *testing.T) {
	for _, action := range []string{"public", "delete"} {
		t.Run(action, func(t *testing.T) {
			f := newAppFixture(t)
			app := createStaticApp(t, f)
			cookie := pairedOwner(t, f)
			r := pairingRequest(http.MethodGet, "/", "192.0.2.1", cookie)
			session, err := f.edge.auth.Browser(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			for _, confirmation := range []string{"", "wrong", app.ID} {
				form := url.Values{"id": {app.ID}, "action": {action}, "csrf": {session.CSRF}, "confirmation": {confirmation}}
				request := httptest.NewRequest(http.MethodPost, ManagementOrigin()+"/action", strings.NewReader(form.Encode()))
				request.Header.Set("Origin", ManagementOrigin())
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				request.AddCookie(cookie)
				response := httptest.NewRecorder()
				f.edge.ServeHost(response, request, ManagementHost())
				if confirmation != app.ID {
					if response.Code != http.StatusBadRequest {
						t.Fatalf("confirmation %q returned %d, want 400", confirmation, response.Code)
					}
					unchanged := f.edge.state.Apps[app.ID]
					if unchanged.Visibility != "private" || unchanged.Status != "active" {
						t.Fatal("unconfirmed mutation changed app")
					}
					continue
				}
				if response.Code != http.StatusSeeOther {
					t.Fatalf("exact confirmation returned %d", response.Code)
				}
				changed := f.edge.state.Apps[app.ID]
				if action == "public" && changed.Visibility != "public" {
					t.Fatal("public confirmation did not apply")
				}
				if action == "delete" && changed.Status == "active" {
					t.Fatal("delete confirmation did not apply")
				}
			}
		})
	}
}
