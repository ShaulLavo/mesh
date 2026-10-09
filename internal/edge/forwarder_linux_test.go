package edge

import (
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProxyForwardedIdentityUsesLiveSocketOwner(t *testing.T) {
	for _, mode := range []string{"allowed", "disallowed", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			registry := testRegistry(t, ModeProxy, now)
			defer registry.Close()
			if mode != "allowed" {
				registry.forwarderTrusted = func(r *http.Request) bool {
					return loopbackForwarderUID(r, func(netip.AddrPort, netip.AddrPort) (uint32, error) {
						if mode == "unknown" {
							return 0, errors.New("owner unavailable")
						}
						uid := uint32(1)
						if os.Getuid() == 1 {
							uid = 2
						}
						return uid, nil
					})
				}
			}
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				_, _ = fmt.Fprint(w, html.EscapeString(r.Header.Get("X-Forwarded-For")+" "+r.Header.Get("X-Forwarded-Proto")))
			}))
			defer backend.Close()
			originID, _ := testIdentity(t)
			if err := registry.Replace([]PublishedRoute{{
				Route:  Route{PublicName: "app.mesh.test", ServiceName: "app"},
				Origin: testResolvedOrigin(originID, testHTTPServerEndpoint(t, backend), now),
			}}); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(registry)
			defer server.Close()
			for i := 0; i <= maximumRequestsPerMinute; i++ {
				request, err := http.NewRequest(http.MethodGet, server.URL+"/app", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Host = "app.mesh.test"
				request.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i+1))
				request.Header.Set("X-Forwarded-Proto", "https")
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				want := http.StatusOK
				expected := fmt.Sprintf("198.51.100.%d https", i+1)
				if mode != "allowed" {
					expected = "127.0.0.1 http"
					if i == maximumRequestsPerMinute {
						want = http.StatusTooManyRequests
						expected = "request limit exceeded"
					}
				}
				if response.StatusCode != want || !strings.Contains(string(body), expected) {
					t.Fatalf("request %d got %d %q, want %d %q", i, response.StatusCode, body, want, expected)
				}
			}
		})
	}
}
