package serve

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateHostRootAndLegacyMount(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.URL.RequestURI()+"|"+r.Header.Get("X-Forwarded-Prefix")) //nolint:gosec // The fixture echoes routing metadata for assertions; no browser renders it.
	}))
	defer upstream.Close()
	_, port, _ := net.SplitHostPort(upstream.Listener.Addr().String())
	registry, err := NewRegistry([]Service{{Name: "platform", Kind: Proxy, Target: port, PrivateHost: "fregat.mesh.test"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host, path, origin, want string
		code                     int
	}{
		{"fregat.mesh.test", "/assets/app.js?v=1", "", "/assets/app.js?v=1|/", 200},
		{"pc.mesh.mesh.test", "/platform/release", "", "/release|/platform", 200},
		{"fregat.mesh.test", "/platform/release?x=1", "", "/release?x=1", 308},
		{"fregat.mesh.test", "/platform//attacker.invalid", "", "/attacker.invalid", 308},
		{"pc.mesh.mesh.test", "/platform?x=1", "", "https://fregat.mesh.test/?x=1", 307},
		{"fregat.mesh.test", "/mutate", "https://attacker.invalid", "", 403},
		{"other.mesh.test", "/release", "", "", 404},
	} {
		t.Run(tc.host+tc.path+tc.origin, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "https://"+tc.host+tc.path, nil)
			if tc.origin != "" {
				request.Header.Set("Origin", tc.origin)
			}
			response := httptest.NewRecorder()
			registry.ServeHTTP(response, request)
			if response.Code != tc.code {
				t.Fatalf("status = %d, want %d; %s", response.Code, tc.code, response.Body.String())
			}
			if (tc.code == 308 || tc.code == 307) && response.Header().Get("Location") != tc.want {
				t.Fatalf("location = %q", response.Header().Get("Location"))
			}
			if tc.code == 200 && response.Body.String() != tc.want {
				t.Fatalf("body = %q, want %q", response.Body.String(), tc.want)
			}
		})
	}
}

func TestPrivateHostConflicts(t *testing.T) {
	for _, services := range [][]Service{
		{{Name: "one", Kind: Proxy, Target: "3301", PrivateHost: "fregat.mesh.test"}, {Name: "two", Kind: Proxy, Target: "3302", PrivateHost: "fregat.mesh.test"}},
		{{Name: "one", Kind: Proxy, Target: "3301", PrivateHost: "fregat.mesh.test", PublicName: "one.mesh.test"}},
		{{Name: "one", Kind: Proxy, Target: "3301", PrivateHost: "fregat.attacker.invalid"}},
		{{Name: "one", Kind: Proxy, Target: "3301", PrivateHost: "fregat.mesh.test"}, {Name: "two", Kind: Proxy, Target: "3302", PublicName: "fregat.mesh.test"}},
		{{Name: "one", Kind: Proxy, Target: "3301", PrivateHost: "apps.mesh.test"}},
		{{Name: "one", Kind: Proxy, Target: "3301", PrivateHost: "abcd.mesh.test"}},
	} {
		if _, err := NewRegistry(services); err == nil {
			t.Fatal("invalid private host accepted")
		}
	}
}

func TestPrivateHostStaticAndCanonicalSNI(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("private root"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry([]Service{{Name: "site", Kind: Static, Target: root, PrivateHost: "docs.mesh.test"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, serverName := range []string{"DOCS.MESH.TEST.", "sibling.mesh.test"} {
		request := httptest.NewRequest(http.MethodGet, "https://docs.mesh.test/", nil)
		request.TLS = &tls.ConnectionState{ServerName: serverName}
		response := httptest.NewRecorder()
		registry.ServeHTTP(response, request)
		if serverName == "sibling.mesh.test" {
			if response.Code != http.StatusMisdirectedRequest {
				t.Fatalf("mismatch status = %d", response.Code)
			}
			continue
		}
		if response.Code != http.StatusOK || response.Body.String() != "private root" {
			t.Fatalf("static root = %d %q", response.Code, response.Body.String())
		}
	}
}
