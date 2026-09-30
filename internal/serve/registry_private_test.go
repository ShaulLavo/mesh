package serve

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRegistryPrivateCrossSitePOST(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	_, port, err := net.SplitHostPort(upstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry([]Service{{Name: "api", Kind: Proxy, Target: port}})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewTLSServer(registry)
	defer front.Close()
	request, err := http.NewRequest(http.MethodPost, front.URL+"/api/mutate", strings.NewReader("confirm=yes"))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "pc.mesh.shaulavo.dev"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	request.Header.Set("Sec-Fetch-Mode", "no-cors")
	response, err := front.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusForbidden || hits.Load() != 0 {
		t.Fatalf("private cross-site POST status=%d, upstream hits=%d; want 403 and zero", response.StatusCode, hits.Load())
	}
}
