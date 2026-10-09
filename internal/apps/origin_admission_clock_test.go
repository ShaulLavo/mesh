package apps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdmissionCacheRejectsStaleTimeAfterUnrelatedExpiry(t *testing.T) {
	o := &Origin{admissions: map[string]*admissionCache{}}
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	deadline := base.Add(30 * time.Second)
	proof := Signed{ID: "first-token"}
	a := admission{ID: "aaaa", Until: deadline}
	if got := o.consumeAdmission(proof, a, base); got != admissionAccepted {
		t.Fatalf("first consumption returned %d, want accepted", got)
	}
	other := admission{ID: "bbbb", Until: deadline.Add(30 * time.Second)}
	if got := o.consumeAdmission(Signed{ID: "unrelated-token"}, other, deadline); got != admissionAccepted {
		t.Fatalf("unrelated consumption returned %d, want accepted", got)
	}
	if _, exists := o.admissions[a.ID]; exists {
		t.Fatal("unrelated admission did not expire the first token")
	}
	if got := o.consumeAdmission(proof, a, deadline.Add(-time.Nanosecond)); got != admissionReplayed {
		t.Fatalf("stale-time replay returned %d, want replayed", got)
	}
}

func TestOriginAdmissionClockRollbackDoesNotReopenReplay(t *testing.T) {
	f := newAppFixture(t)
	app := createAdmissionApp(t, f)
	other := createAdmissionApp(t, f)
	deadline := f.now.Add(30 * time.Second)
	r := signedAdmissionRequest(t, f, app, deadline, false)
	if w := serveAdmission(t, f.origin, r); w.Code != http.StatusOK {
		t.Fatalf("first consumption returned %d, want 200", w.Code)
	}
	f.now = deadline
	fresh := signedAdmissionRequest(t, f, other, f.now.Add(30*time.Second), false)
	if w := serveAdmission(t, f.origin, fresh); w.Code != http.StatusOK {
		t.Fatalf("unrelated admission returned %d, want 200", w.Code)
	}
	if _, exists := f.origin.admissions[app.ID]; exists {
		t.Fatal("unrelated admission did not expire the first token")
	}
	f.now = deadline.Add(-time.Nanosecond)
	if w := serveAdmission(t, f.origin, r); w.Code != http.StatusNotFound {
		t.Fatalf("clock rollback reopened replay: got %d, want 404", w.Code)
	}
	fresh = signedAdmissionRequest(t, f, app, f.now.Add(30*time.Second), false)
	if w := serveAdmission(t, f.origin, fresh); w.Code != http.StatusOK {
		t.Fatalf("clock rollback refused a fresh live token: got %d, want 200", w.Code)
	}
}

func TestEdgeAdmissionUsesOneSigningTime(t *testing.T) {
	for _, download := range []bool{false, true} {
		name := "view"
		if download {
			name = "download"
		}
		t.Run(name, func(t *testing.T) {
			f := newAppFixture(t)
			app := createAdmissionApp(t, f)
			forwarded := make(chan Signed, 1)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				encoded, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Mesh-App-Admission"))
				if err != nil {
					http.Error(w, "bad admission encoding", http.StatusBadRequest)
					return
				}
				var proof Signed
				if err := json.Unmarshal(encoded, &proof); err != nil {
					http.Error(w, "bad admission proof", http.StatusBadRequest)
					return
				}
				forwarded <- proof
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
			f.edge.config.ClientIP = func(*http.Request) netip.Addr { return netip.MustParseAddr("100.64.0.2") }
			f.edge.config.NetworkOwners = func(context.Context, netip.Addr) ([]string, error) {
				return []string{identityFor(f.ownerKey)}, nil
			}
			var calls atomic.Int64
			f.edge.config.Now = func() time.Time {
				return f.now.Add(-time.Duration(calls.Add(1)) * time.Nanosecond)
			}
			host := app.ID + "." + Domain()
			target := URL(app.ID) + "/"
			if download {
				host = ManagementHost()
				target = ManagementOrigin() + "/download?id=" + app.ID
			}
			w := httptest.NewRecorder()
			if !f.edge.ServeHost(w, httptest.NewRequest(http.MethodGet, target, nil), host) {
				t.Fatal("edge did not handle app request")
			}
			var proof Signed
			select {
			case proof = <-forwarded:
			default:
				t.Fatalf("edge did not forward admission: status %d", w.Code)
			}
			var a admission
			if err := json.Unmarshal(proof.Body, &a); err != nil {
				t.Fatal(err)
			}
			if lifetime := a.Until.Sub(proof.IssuedAt); lifetime != 30*time.Second {
				t.Fatalf("signing clock rollback changed token lifetime: got %s, want 30s", lifetime)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("clock rollback while signing returned %d, want 200", w.Code)
			}
		})
	}
}

func TestAdmissionCacheStoresWallTimeHighWaterMark(t *testing.T) {
	o := &Origin{admissions: map[string]*admissionCache{}}
	now := time.Now()
	a := admission{ID: "aaaa", Until: now.Add(30 * time.Second).UTC()}
	if got := o.consumeAdmission(Signed{ID: "first-token"}, a, now); got != admissionAccepted {
		t.Fatalf("first consumption returned %d, want accepted", got)
	}
	if o.admissionAt != o.admissionAt.Round(0) {
		t.Fatal("wall-time high-water mark retained a local monotonic timestamp")
	}
}
