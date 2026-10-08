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

func TestPrivateHostRootWithdrawsMachineMount(t *testing.T) {
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
		{"pc.mesh.mesh.test", "/platform/release", "", "", 404},
		{"fregat.mesh.test", "/platform/release?x=1", "", "/release?x=1", 307},
		{"fregat.mesh.test", "/platform//attacker.invalid", "", "/attacker.invalid", 307},
		{"pc.mesh.mesh.test", "/platform?x=1", "", "", 404},
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
			if tc.code == http.StatusTemporaryRedirect && response.Header().Get("Location") != tc.want {
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

func TestPrivateHostWithdrawnMountBlocksParentFallback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer upstream.Close()
	_, port, _ := net.SplitHostPort(upstream.Listener.Addr().String())
	registry, err := NewRegistry([]Service{
		{Name: "apps", Kind: Proxy, Target: port},
		{Name: "apps/platform", Kind: Proxy, Target: port, PrivateHost: "fregat.mesh.test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/apps/platform", "/apps/platform/", "/apps/platform/release?x=1"} {
		request := httptest.NewRequest(http.MethodGet, "https://pc.mesh.mesh.test"+path, nil)
		request.Header.Set("Accept", "text/html")
		response := httptest.NewRecorder()
		registry.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound || response.Header().Get("Location") != "" {
			t.Fatalf("%s: status %d, redirect %q", path, response.Code, response.Header().Get("Location"))
		}
	}
	request := httptest.NewRequest(http.MethodGet, "https://pc.mesh.mesh.test/apps/other", nil)
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("parent status = %d", response.Code)
	}
}

func TestPrivateHostWithdrawnMountNeverStartsService(t *testing.T) {
	service := onDemand()
	service.PrivateHost = "console.mesh.test"
	registry, err := NewRegistry([]Service{service})
	if err != nil {
		t.Fatal(err)
	}
	gate := &fakeGate{err: errString("upstream not started")}
	registry.SetDemandGate(gate, nil)
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		request := httptest.NewRequest(method, "https://pc.mesh.mesh.test/"+service.Name+"/socket", nil)
		request.Header.Set("Accept", "text/html")
		request.Header.Set("Upgrade", "websocket")
		request.Header.Set("Connection", "Upgrade")
		response := httptest.NewRecorder()
		registry.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound || len(gate.entered) != 0 || response.Header().Get("Location") != "" {
			t.Fatalf("%s: status %d, starts %v, redirect %q", method, response.Code, gate.entered, response.Header().Get("Location"))
		}
	}
	request := httptest.NewRequest(http.MethodGet, "https://console.mesh.test/socket", nil)
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || len(gate.entered) != 1 || gate.entered[0] != service.Name {
		t.Fatalf("short host: status %d, starts %v", response.Code, gate.entered)
	}
}

func TestServiceEqualityIncludesPrivateHost(t *testing.T) {
	service := Service{Name: "platform", Kind: Proxy, Target: "3301"}
	aliased := service
	aliased.PrivateHost = "fregat.mesh.test"
	if service.Equal(aliased) || aliased.Equal(service) {
		t.Fatal("private host change compared equal")
	}
	identical := aliased
	if !aliased.Equal(identical) {
		t.Fatal("identical private host compared unequal")
	}
}
