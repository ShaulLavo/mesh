package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	meshserve "github.com/shaul/mesh/internal/serve"
)

func TestPrivateHTTPRejectsRebindingHost(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "private-file.txt"), []byte("private fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := meshserve.NewRegistry([]meshserve.Service{{Name: "files", Kind: meshserve.Files, Target: root}})
	if err != nil {
		t.Fatal(err)
	}
	for _, surface := range []string{"tailnet HTTP", "loopback HTTPS"} {
		t.Run(surface, func(t *testing.T) {
			var handler http.Handler
			if surface == "tailnet HTTP" {
				handler = newWebSocketServer(context.Background(), listenerConfig{
					webSocketPath: "/mesh", httpHandler: appOriginHandler(nil, registry),
					tailnetAddrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}, tailnetPort: 7337,
				}, newConnectionGroup(echoOneFrame)).Handler
			} else {
				handler = serviceOnlyHTTPSHandler("/mesh", appOriginHandler(nil, registry))
			}
			server := httptest.NewUnstartedServer(handler)
			if surface == "loopback HTTPS" {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/files/", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Host = "rebind.attacker.example:7337"
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close() //nolint:errcheck // test response cleanup
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			listing := strings.Contains(string(body), "private-file.txt")
			t.Logf("Host %q returned %d; private Files listing exposed = %t", request.Host, response.StatusCode, listing)
			if response.StatusCode != http.StatusMisdirectedRequest || listing {
				t.Fatalf("rebinding response = %d, private listing = %t; want 421 and no listing", response.StatusCode, listing)
			}
		})
	}
}
