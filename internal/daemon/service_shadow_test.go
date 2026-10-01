package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
)

func TestServiceRegistrationWarnsWhenPrivateRouteShadowsPublicParent(t *testing.T) {
	for _, privateFirst := range []bool{false, true} {
		name := "public first"
		if privateFirst {
			name = "private first"
		}
		t.Run(name, func(t *testing.T) {
			_, registry, controller := newServiceControllerTest(t, "/control")
			publicRoot, privateRoot := t.TempDir(), t.TempDir()
			for root, body := range map[string]string{publicRoot: "public", privateRoot: "private"} {
				if err := os.WriteFile(filepath.Join(root, "index.html"), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			services := []protocol.ServiceInfo{
				{Name: "blog", Kind: "static", Target: publicRoot, PublicName: "blog.shaulavo.dev"},
				{Name: "blog/admin", Kind: "static", Target: privateRoot},
			}
			if privateFirst {
				services[0], services[1] = services[1], services[0]
			}
			for index, service := range services {
				response, _, err := controller.HandleControl(context.Background(), protocol.Control{
					Type: protocol.TypeServiceUpsert, RequestID: "shadow", Service: &service,
				})
				if err != nil || response.Type != protocol.TypeServiceUpserted {
					t.Fatalf("register %s: %s, %v", service.Name, response.Type, err)
				}
				if index == 0 && response.Message != "" {
					t.Errorf("first registration has a shadow warning: %q", response.Message)
				}
				if index == 1 {
					for _, want := range []string{"private route /blog/admin", "shadows public route https://blog.shaulavo.dev/blog", "404"} {
						if !strings.Contains(response.Message, want) {
							t.Errorf("registration warning %q does not contain %q", response.Message, want)
						}
					}
				}
			}
			assertServiceResponse(t, registry, "https://blog.shaulavo.dev/blog/", http.StatusOK, "public")
			assertServiceResponse(t, registry, "https://blog.shaulavo.dev/blog/admin/", http.StatusNotFound, "404 page not found\n")
			assertServiceResponse(t, registry, "http://pc.mesh.shaulavo.dev/blog/admin/", http.StatusOK, "private")
		})
	}
}
