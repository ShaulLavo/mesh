package apps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func createAdmissionApp(t *testing.T, f *appFixture, public bool) Record {
	t.Helper()
	uploadID, digest := uploadSource(t, f, sourceFixture(t))
	result, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "static", UploadID: uploadID, Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	if public {
		result, err = f.origin.Handle(context.Background(), Request{Action: "public", ID: result.App.ID})
		if err != nil {
			t.Fatal(err)
		}
	}
	return *result.App
}

func signedAdmissionRequest(t *testing.T, f *appFixture, app Record, until time.Time, download bool) *http.Request {
	t.Helper()
	proof, err := Sign("mesh-app/admission/v1", identityFor(f.ownerKey), app.Generation, admission{
		ID: app.ID, Generation: app.Generation, Method: http.MethodGet, URI: "/",
		Host: app.ID + "." + Domain, Until: until, Download: download,
	}, f.edgeKey, f.now)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "http://origin/.mesh-app/origin/"+app.ID+"/", nil)
	r.Header.Set("X-Mesh-App-Admission", base64.RawURLEncoding.EncodeToString(encoded))
	return r
}

func serveAdmission(t *testing.T, o *Origin, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	if !o.ServeHTTP(w, r) {
		t.Fatal("origin did not handle signed admission")
	}
	return w
}

func fillViewAdmissions(t *testing.T, f *appFixture, app Record) *http.Request {
	t.Helper()
	var first *http.Request
	for i := range 4096 {
		r := signedAdmissionRequest(t, f, app, f.now.Add(30*time.Second), false)
		if i == 0 {
			first = r
		}
		if w := serveAdmission(t, f.origin, r); w.Code != http.StatusOK {
			t.Fatalf("view admission %d returned %d, want 200", i, w.Code)
		}
	}
	return first
}

func TestOriginAdmissionFloodIsolatedFromOtherAppsAndOwner(t *testing.T) {
	f := newAppFixture(t)
	flooded := createAdmissionApp(t, f, true)
	other := createAdmissionApp(t, f, true)
	private := createAdmissionApp(t, f, false)
	first := fillViewAdmissions(t, f, flooded)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.origin.ServeHTTP(w, r) {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(origin.Close)
	endpoint, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	f.edge.config.Resolve = func(context.Context, string) (netip.AddrPort, error) {
		return netip.ParseAddrPort(endpoint.Host)
	}
	t.Run("unrelated public app", func(t *testing.T) {
		w := httptest.NewRecorder()
		if !f.edge.ServeHost(w, httptest.NewRequest(http.MethodGet, URL(other.ID)+"/", nil), other.ID+"."+Domain) || w.Code != http.StatusOK {
			t.Fatalf("unrelated public app returned %d, want 200", w.Code)
		}
		if !strings.Contains(w.Body.String(), "original page") {
			t.Fatal("unrelated public app did not serve its content")
		}
	})
	t.Run("owner private app", func(t *testing.T) {
		f.edge.config.ClientIP = func(*http.Request) netip.Addr { return netip.MustParseAddr("100.64.0.2") }
		f.edge.config.NetworkOwners = func(context.Context, netip.Addr) ([]string, error) {
			return []string{identityFor(f.ownerKey)}, nil
		}
		w := httptest.NewRecorder()
		if !f.edge.ServeHost(w, httptest.NewRequest(http.MethodGet, URL(private.ID)+"/", nil), private.ID+"."+Domain) || w.Code != http.StatusOK {
			t.Fatalf("owner private app returned %d, want 200", w.Code)
		}
		if !strings.Contains(w.Body.String(), "original page") {
			t.Fatal("owner private app did not serve its content")
		}
	})
	t.Run("owner download reserve", func(t *testing.T) {
		r := signedAdmissionRequest(t, f, flooded, f.now.Add(30*time.Second), true)
		if w := serveAdmission(t, f.origin, r); w.Code != http.StatusOK {
			t.Fatalf("owner download returned %d, want 200", w.Code)
		}
		if w := serveAdmission(t, f.origin, r); w.Code != http.StatusNotFound {
			t.Fatalf("replayed owner download returned %d, want 404", w.Code)
		}
	})
	t.Run("honest overload", func(t *testing.T) {
		w := serveAdmission(t, f.origin, signedAdmissionRequest(t, f, flooded, f.now.Add(30*time.Second), false))
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
			t.Fatalf("full app returned %d with Retry-After %q, want 503 with 1", w.Code, w.Header().Get("Retry-After"))
		}
	})
	t.Run("replay at capacity", func(t *testing.T) {
		if w := serveAdmission(t, f.origin, first); w.Code != http.StatusNotFound {
			t.Fatalf("replayed view at capacity returned %d, want 404", w.Code)
		}
	})
}
