package daemon

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/apps"
	meshserve "github.com/shaul/mesh/internal/serve"
)

func TestAppOriginHandlerRejectsPublicAccessToNestedPrivateService(t *testing.T) {
	for _, withApps := range []bool{false, true} {
		name := "services only"
		if withApps {
			name = "app fallback"
		}
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				_, _ = io.WriteString(w, "PRIVATE ADMIN")
			}))
			defer upstream.Close()
			_, port, err := net.SplitHostPort(upstream.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			store, registry, _ := newServiceControllerTest(t, "/mesh")
			if err := registry.Replace([]meshserve.Service{
				{Name: "app", Kind: meshserve.Proxy, Target: port, PublicName: "app.shaulavo.dev"},
				{Name: "app/admin", Kind: meshserve.Proxy, Target: port},
			}); err != nil {
				t.Fatal(err)
			}
			var origin *apps.Origin
			if withApps {
				origin, _ = protectedTestOrigin(t, store, registry, t.TempDir(), 0)
			}
			server := httptest.NewServer(appOriginHandler(origin, registry))
			defer server.Close()
			for _, test := range []struct {
				host       string
				wantStatus int
				wantHits   int32
			}{
				{host: "app.shaulavo.dev", wantStatus: http.StatusNotFound},
				{host: "pc.mesh.shaulavo.dev", wantStatus: http.StatusOK, wantHits: 1},
			} {
				request, err := http.NewRequest(http.MethodGet, server.URL+"/app/admin/secrets", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Host = test.host
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != test.wantStatus || hits.Load() != test.wantHits {
					t.Fatalf("Host %q reached nested private service: status=%d upstream hits=%d body=%q, want status=%d hits=%d", test.host, response.StatusCode, hits.Load(), body, test.wantStatus, test.wantHits)
				}
			}
		})
	}
}
