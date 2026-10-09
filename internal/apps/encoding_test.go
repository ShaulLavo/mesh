package apps

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
)

func TestAppProxyPreservesCompressedHTMLWithoutWidget(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	var body bytes.Buffer
	compressed := gzip.NewWriter(&body)
	if _, err := compressed.Write([]byte("<html><head></head><body>app</body></html>")); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if accepted := r.Header.Get("Accept-Encoding"); accepted != "gzip, deflate, br, zstd" {
			t.Errorf("upstream negotiation changed: %q", accepted)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.Header().Set("ETag", `"app-html"`)
		_, _ = w.Write(body.Bytes())
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
	f.edge.ServeHost(result, ownerRequest(f, request), app.ID+"."+Domain())
	if result.Code != http.StatusOK || !bytes.Equal(result.Body.Bytes(), body.Bytes()) {
		t.Fatalf("compressed HTML changed: status=%d body=%q", result.Code, result.Body.Bytes())
	}
	for key, want := range map[string]string{"Content-Encoding": "gzip", "Content-Length": strconv.Itoa(body.Len()), "Content-Security-Policy": "default-src 'none'", "ETag": `"app-html"`} {
		if got := result.Header().Get(key); got != want {
			t.Errorf("%s changed: got %q, want %q", key, got, want)
		}
	}
}
