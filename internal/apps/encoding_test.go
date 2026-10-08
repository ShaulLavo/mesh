package apps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestAppProxyNegotiatesSupportedEncodingWithZstdPreferringOrigin(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	if _, err := f.origin.Handle(context.Background(), Request{Action: "public", ID: app.ID}); err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepted := r.Header.Get("Accept-Encoding")
		if accepted != "identity, gzip;q=0.8, br;q=0.8" {
			t.Errorf("upstream negotiation=%q, want supported encodings with identity preferred", accepted)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if strings.Contains(accepted, "zstd") {
			w.Header().Set("Content-Encoding", "zstd")
			_, _ = w.Write([]byte{0x28, 0xb5, 0x2f, 0xfd, 0x20, 0x2a, 0x51, 0x01, 0x00})
		}
		_, _ = w.Write([]byte("<html><head></head><body>app</body></html>"))
	}))
	t.Cleanup(origin.Close)
	endpoint, err := netip.ParseAddrPort(strings.TrimPrefix(origin.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	f.edge.config.Resolve = func(context.Context, string) (netip.AddrPort, error) { return endpoint, nil }
	request := httptest.NewRequest(http.MethodGet, URL(app.ID), nil)
	request.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	result := httptest.NewRecorder()
	f.edge.ServeHost(result, request, app.ID+"."+Domain())
	if result.Code != http.StatusOK || result.Header().Get("Content-Encoding") != "" || !strings.Contains(result.Body.String(), "data-mesh-app=") {
		t.Fatalf("zstd-preferring origin lost pill: status=%d, encoding=%q", result.Code, result.Header().Get("Content-Encoding"))
	}
}
