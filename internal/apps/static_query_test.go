package apps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestStaticAppResolvesPathThroughRegistry(t *testing.T) {
	f := newAppFixture(t)
	source := sourceFixture(t)
	for _, name := range []string{"hello world.html", "question?.html", "hash#.html"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte("original page"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	upload, digest := uploadSource(t, f, source)
	result, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "static", UploadID: upload, Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	app := *result.App
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
	for _, target := range []string{
		"/index.html", "/index.html?", "/index.html?a=1", "/index.html?file=../secret&escape=%zz",
		"/?", "/hello%20world.html?", "/question%3F.html?", "/hash%23.html?a=1",
	} {
		t.Run(target, func(t *testing.T) {
			r := ownerRequest(f, httptest.NewRequest(http.MethodGet, URL(app.ID)+target, nil))
			r.URL.Fragment = "ignored"
			w := httptest.NewRecorder()
			if !f.edge.ServeHost(w, r, app.ID+"."+Domain()) {
				t.Fatal("registry did not handle app")
			}
			if w.Code != http.StatusOK || w.Body.String() != "original page" {
				t.Fatalf("GET %s = %d %q, want 200 original page", target, w.Code, w.Body.String())
			}
		})
	}
	for _, target := range []string{"/../secret?", "/%2e%2e/secret?a=1", "/%252e%252e/secret?", "/%00?"} {
		t.Run(target, func(t *testing.T) {
			r := ownerRequest(f, httptest.NewRequest(http.MethodGet, URL(app.ID)+target, nil))
			w := httptest.NewRecorder()
			f.edge.ServeHost(w, r, app.ID+"."+Domain())
			if w.Code != http.StatusNotFound && w.Code != http.StatusBadRequest {
				t.Fatalf("unsafe path %s returned %d", target, w.Code)
			}
		})
	}
}

func TestOriginAdmissionBindsEmptyQueryMarker(t *testing.T) {
	f := newAppFixture(t)
	app := createAdmissionApp(t, f)
	r := signedAdmissionRequest(t, f, app, f.now.Add(admissionLifetime), false)
	r.URL.ForceQuery = true
	if w := serveAdmission(t, f.origin, r); w.Code != http.StatusNotFound {
		t.Fatalf("modified admission returned %d, want 404", w.Code)
	}
}
