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
			registry := testRegistry(t, ModeProxy, time.Now())
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
			registry.SetAppHandler(appHandlerFunc(func(w http.ResponseWriter, r *http.Request, _ string) bool {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				ip := r.Context().Value(proxyClientIPKey{}).(netip.Addr)
				_, _ = fmt.Fprint(w, html.EscapeString(fmt.Sprintf("%s %s", ip, registry.forwardedScheme(r))))
				return true
			}))
			server := httptest.NewServer(registry)
			defer server.Close()
			for i := 0; i <= maximumRequestsPerMinute; i++ {
				request, err := http.NewRequest(http.MethodGet, server.URL+"/", nil)
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
