package apps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"runtime"
	"strings"
	"sync"
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

func TestAdmissionCacheBoundedUnderFlood(t *testing.T) {
	o := &Origin{admissions: map[string]*admissionCache{}}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for app := range maxAdmissionApps {
		id := fmt.Sprintf("%04x", app)
		for i := range maxViewAdmissions + maxDownloadAdmissions {
			proof := Signed{ID: fmt.Sprintf("%043d", app*(maxViewAdmissions+maxDownloadAdmissions)+i)}
			a := admission{ID: id, Until: now.Add(30 * time.Second), Download: i >= maxViewAdmissions}
			if got := o.consumeAdmission(proof, a, now); got != admissionAccepted {
				t.Fatalf("app %s admission %d returned %d, want accepted", id, i, got)
			}
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	t.Logf("hard capacity retains %d proof IDs using %.1f MiB of heap", maxAdmissionApps*(maxViewAdmissions+maxDownloadAdmissions), float64(after.HeapAlloc-before.HeapAlloc)/(1<<20))
	for second := range 30 {
		at := now.Add(time.Duration(second) * time.Second)
		for app := range maxAdmissionApps {
			id := fmt.Sprintf("%04x", app)
			for _, download := range []bool{false, true} {
				a := admission{ID: id, Until: at.Add(30 * time.Second), Download: download}
				if got := o.consumeAdmission(Signed{ID: "fresh-overflow"}, a, at); got != admissionFull {
					t.Fatalf("full app %s returned %d, want full", id, got)
				}
				first := app * (maxViewAdmissions + maxDownloadAdmissions)
				if download {
					first += maxViewAdmissions
				}
				if got := o.consumeAdmission(Signed{ID: fmt.Sprintf("%043d", first)}, a, at); got != admissionReplayed {
					t.Fatalf("app %s lost live replay protection at %s", id, at)
				}
			}
		}
		if got := o.consumeAdmission(Signed{ID: "new-app"}, admission{ID: "zzzz", Until: at.Add(30 * time.Second)}, at); got != admissionFull {
			t.Fatalf("cache churn exceeded app count bound at %s", at)
		}
		entries := 0
		for id, cache := range o.admissions {
			if len(cache.seen) != len(cache.deadlines) || cache.views != maxViewAdmissions || cache.downloads != maxDownloadAdmissions {
				t.Fatalf("app %s cache counts diverged under flood", id)
			}
			entries += len(cache.seen)
		}
		if entries != maxAdmissionApps*(maxViewAdmissions+maxDownloadAdmissions) {
			t.Fatalf("retained %d proof IDs, want hard bound", entries)
		}
	}
	at := now.Add(30 * time.Second)
	if got := o.consumeAdmission(Signed{ID: "new-app"}, admission{ID: "zzzz", Until: at.Add(30 * time.Second)}, at); got != admissionAccepted {
		t.Fatalf("new app refused after all deadlines expired: %d", got)
	}
	if len(o.admissions) != 1 || len(o.admissions["zzzz"].seen) != 1 {
		t.Fatal("expired app caches were retained")
	}
}

func TestOriginAdmissionExpiresAtItsOwnDeadline(t *testing.T) {
	f := newAppFixture(t)
	app := createAdmissionApp(t, f, true)
	base := f.now
	long := signedAdmissionRequest(t, f, app, base.Add(30*time.Second), false)
	if w := serveAdmission(t, f.origin, long); w.Code != http.StatusOK {
		t.Fatalf("long admission returned %d", w.Code)
	}
	short := signedAdmissionRequest(t, f, app, base.Add(15*time.Nanosecond), false)
	if w := serveAdmission(t, f.origin, short); w.Code != http.StatusOK {
		t.Fatalf("short admission returned %d", w.Code)
	}
	for range maxViewAdmissions - 2 {
		r := signedAdmissionRequest(t, f, app, base.Add(20*time.Millisecond), false)
		if w := serveAdmission(t, f.origin, r); w.Code != http.StatusOK {
			t.Fatalf("admission returned %d before capacity", w.Code)
		}
	}
	f.now = base.Add(14 * time.Nanosecond)
	if w := serveAdmission(t, f.origin, signedAdmissionRequest(t, f, app, f.now.Add(30*time.Second), false)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("live entry evicted before its deadline: %d", w.Code)
	}
	f.now = base.Add(15 * time.Nanosecond)
	if w := serveAdmission(t, f.origin, short); w.Code != http.StatusNotFound {
		t.Fatalf("expired proof returned %d, want 404", w.Code)
	}
	if w := serveAdmission(t, f.origin, signedAdmissionRequest(t, f, app, f.now.Add(30*time.Second), false)); w.Code != http.StatusOK {
		t.Fatalf("own deadline did not free capacity: %d", w.Code)
	}
	if w := serveAdmission(t, f.origin, long); w.Code != http.StatusNotFound {
		t.Fatalf("live long proof was evicted: %d", w.Code)
	}
	f.now = base.Add(20 * time.Millisecond)
	if w := serveAdmission(t, f.origin, signedAdmissionRequest(t, f, app, f.now.Add(30*time.Second), false)); w.Code != http.StatusOK {
		t.Fatalf("out-of-order deadlines did not expire: %d", w.Code)
	}
	if got := len(f.origin.admissions[app.ID].seen); got != 3 {
		t.Fatalf("retained %d IDs after staggered expiry, want 3", got)
	}
}

func TestOriginAdmissionDownloadCapacityDoesNotBlockViews(t *testing.T) {
	f := newAppFixture(t)
	app := createAdmissionApp(t, f, true)
	var first *http.Request
	for i := range maxDownloadAdmissions {
		r := signedAdmissionRequest(t, f, app, f.now.Add(30*time.Second), true)
		if i == 0 {
			first = r
		}
		if w := serveAdmission(t, f.origin, r); w.Code != http.StatusOK {
			t.Fatalf("download %d returned %d, want 200", i, w.Code)
		}
	}
	w := serveAdmission(t, f.origin, signedAdmissionRequest(t, f, app, f.now.Add(30*time.Second), true))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("full download reserve returned %d with Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if w := serveAdmission(t, f.origin, first); w.Code != http.StatusNotFound {
		t.Fatalf("download replay at capacity returned %d", w.Code)
	}
	if w := serveAdmission(t, f.origin, signedAdmissionRequest(t, f, app, f.now.Add(30*time.Second), false)); w.Code != http.StatusOK {
		t.Fatalf("download capacity blocked view: %d", w.Code)
	}
}

func TestOriginAdmissionRejectsInvalidRoutesAndLongDeadlinesWithoutCaching(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*appFixture, *Record)
		until  time.Duration
		status int
	}{
		{name: "unknown app", change: func(_ *appFixture, a *Record) { a.ID = "zzzz" }, until: 30 * time.Second, status: http.StatusServiceUnavailable},
		{name: "wrong generation", change: func(_ *appFixture, a *Record) { a.Generation++ }, until: 30 * time.Second, status: http.StatusServiceUnavailable},
		{name: "lapsed lease", change: func(f *appFixture, _ *Record) { f.now = f.now.Add(LeaseTTL) }, until: 30 * time.Second, status: http.StatusServiceUnavailable},
		{name: "long deadline", change: func(*appFixture, *Record) {}, until: 30*time.Second + time.Nanosecond, status: http.StatusNotFound},
		{name: "expired deadline", change: func(*appFixture, *Record) {}, until: 0, status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppFixture(t)
			app := createAdmissionApp(t, f, true)
			tc.change(f, &app)
			w := serveAdmission(t, f.origin, signedAdmissionRequest(t, f, app, f.now.Add(tc.until), false))
			if w.Code != tc.status {
				t.Fatalf("invalid admission returned %d, want %d", w.Code, tc.status)
			}
			if len(f.origin.admissions) != 0 {
				t.Fatal("invalid admission allocated replay state")
			}
		})
	}
}

func TestOriginAdmissionConcurrentReplay(t *testing.T) {
	f := newAppFixture(t)
	app := createAdmissionApp(t, f, true)
	r := signedAdmissionRequest(t, f, app, f.now.Add(30*time.Second), false)
	var wg sync.WaitGroup
	results := make(chan int, 32)
	for range cap(results) {
		wg.Go(func() {
			w := httptest.NewRecorder()
			f.origin.ServeHTTP(w, r.Clone(context.Background()))
			results <- w.Code
		})
	}
	wg.Wait()
	close(results)
	accepted := 0
	for status := range results {
		if status == http.StatusOK {
			accepted++
		} else if status != http.StatusNotFound {
			t.Fatalf("concurrent replay returned %d", status)
		}
	}
	if accepted != 1 || len(f.origin.admissions[app.ID].seen) != 1 {
		t.Fatalf("concurrent replay admitted %d requests, want 1", accepted)
	}
}
