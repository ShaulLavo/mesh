package apps

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestServerAppReceivesRawQueryThroughRegistry(t *testing.T) {
	f := newAppFixture(t)
	workers := newServerWorkers(t, f)
	app := createServerApp(t, f)
	upstream := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.RequestURI) //nolint:gosec // the test upstream echoes what it received
	})}
	go func() { _ = upstream.Serve(workerListener(t, workers, app)) }()
	t.Cleanup(func() { _ = upstream.Close() })
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
	manyParams := "/?" + strings.Repeat("a=1&", 10_000) + "a=1"
	for _, target := range []string{"/?a=1", "/?a=1;b=2", "/?x=%zz&y=%", "/?&&==", "/?%3F", manyParams} {
		name := target
		if len(name) > 32 {
			name = name[:32] + "…"
		}
		t.Run(name, func(t *testing.T) {
			r := ownerRequest(f, httptest.NewRequest(http.MethodGet, URL(app.ID)+target, nil))
			w := httptest.NewRecorder()
			if !f.edge.ServeHost(w, r, app.ID+"."+Domain()) {
				t.Fatal("registry did not handle app")
			}
			if w.Code != http.StatusOK || w.Body.String() != target {
				got := w.Body.String()
				if len(got) > 64 {
					got = got[:64] + "…"
				}
				t.Fatalf("GET %s = %d, upstream saw %q", name, w.Code, got)
			}
		})
	}
}
