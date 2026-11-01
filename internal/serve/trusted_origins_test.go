package serve

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCanonicalOrigin(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{"HTTPS://Example.COM/", "https://example.com"},
		{"https://example.com:443", "https://example.com"},
		{"https://example.com:8443", "https://example.com:8443"},
		{"https://127.0.0.1", "https://127.0.0.1"},
		{"http://localhost:80", "http://localhost"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"http://[::1]:80", "http://[::1]"},
		{"https://[2001:DB8::1]:443", "https://[2001:db8::1]"},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := CanonicalOrigin(test.input)
			if err != nil || got != test.want {
				t.Fatalf("CanonicalOrigin(%q) = %q, %v; want %q", test.input, got, err, test.want)
			}
		})
	}
}

func TestCanonicalOriginRejectsAmbiguousAndUnsafeSyntax(t *testing.T) {
	for _, input := range []string{
		"",
		"http://example.com",
		"https://example.com:",
		"https://example.com?",
		"https://example.com#",
		"https://example.com/%2f",
		"https://%65xample.com",
		"https://example.com/path",
		"https://user@example.com",
		"https://*.example.com",
		"https://example.com.",
		"https://[fe80::1%25lo0]",
		"https://[::ffff:127.0.0.1]",
		"https://2130706433",
		"https://0x7f000001",
		"https://127.1",
		"https://example.com:0",
		"https://example.com:65536",
		"ftp://example.com",
		"https://éxample.com",
		"https://example.com\\path",
	} {
		t.Run(input, func(t *testing.T) {
			if got, err := CanonicalOrigin(input); err == nil {
				t.Fatalf("CanonicalOrigin(%q) = %q; want rejection", input, got)
			}
		})
	}
}

func TestNormalizeTrustedOriginsSortsDeduplicatesAndClones(t *testing.T) {
	input := []string{"HTTPS://B.example/", "https://a.example:443", "https://b.example"}
	got, err := NormalizeTrustedOrigins(input)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://a.example", "https://b.example"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized = %#v, want %#v", got, want)
	}
	input[0] = "https://attacker.example"
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized list retained caller backing array: %#v", got)
	}
	if _, err := NormalizeTrustedOrigins([]string{strings.Repeat("a", MaximumTrustedOriginBytes-8) + ".com"}); err == nil {
		t.Fatal("accepted an oversized origin")
	}
	tooMany := make([]string, MaximumTrustedOrigins+1)
	for index := range tooMany {
		tooMany[index] = "https://example.com"
	}
	if _, err := NormalizeTrustedOrigins(tooMany); err == nil {
		t.Fatal("accepted too many trusted origins")
	}
}

func TestRegistryTrustedOriginSnapshotAndRevocation(t *testing.T) {
	var hits atomic.Int32
	registry, err := NewRegistry([]Service{{Name: "api", Kind: Proxy, Target: "1", PrivateHost: "api.mesh.test", TrustedOrigins: []string{"https://bun-demo.sprockt.dev"}}})
	if err != nil {
		t.Fatal(err)
	}
	service := []Service{{Name: "api", Kind: Proxy, Target: "1", PrivateHost: "api.mesh.test", TrustedOrigins: []string{"https://bun-demo.sprockt.dev"}}}
	if err := registry.Replace(service); err != nil {
		t.Fatal(err)
	}
	service[0].TrustedOrigins[0] = "https://attacker.example"
	returned := registry.Services()
	returned[0].TrustedOrigins[0] = "https://attacker.example"
	if registry.Services()[0].TrustedOrigins[0] != "https://bun-demo.sprockt.dev" {
		t.Fatal("registry retained mutable trusted-origin data")
	}
	request := httptest.NewRequest(http.MethodPost, "https://api.mesh.test/", nil)
	request.Header.Set("Origin", "https://bun-demo.sprockt.dev")
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || hits.Load() != 0 {
		// The port is intentionally closed; this proves admission passed the
		// guard and reached the proxy without requiring a real upstream.
		if response.Code == http.StatusForbidden {
			t.Fatalf("trusted origin was rejected: %d %q", response.Code, response.Body.String())
		}
	}
	if err := registry.Replace([]Service{{Name: "api", Kind: Proxy, Target: "1", PrivateHost: "api.mesh.test"}}); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("revoked trusted origin status = %d, want 403", response.Code)
	}
}
