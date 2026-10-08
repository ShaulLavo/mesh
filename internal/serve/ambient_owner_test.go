package serve

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"testing"
)

func TestPrivateRequestOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, host, want string
		tls              bool
	}{
		{name: "private HTTPS", host: "pc.mesh.mesh.test", tls: true, want: "https://pc.mesh.mesh.test"},
		{name: "HTTPS default port", host: "pc.mesh.mesh.test:443", tls: true, want: "https://pc.mesh.mesh.test"},
		{name: "HTTP default port", host: "pc.mesh.mesh.test:80", want: "http://pc.mesh.mesh.test"},
		{name: "tailnet IP and port", host: "100.64.0.1:7337", want: "http://100.64.0.1:7337"},
		{name: "custom HTTPS port", host: "pc.mesh.mesh.test:8443", tls: true, want: "https://pc.mesh.mesh.test:8443"},
		{name: "IPv6 default port", host: "[fd7a:115c:a1e0::1]:443", tls: true, want: "https://[fd7a:115c:a1e0::1]"},
		{name: "IPv6 custom port", host: "[fd7a:115c:a1e0::1]:7337", want: "http://[fd7a:115c:a1e0::1]:7337"},
		{name: "case-insensitive host", host: "PC.Mesh.mesh.test", tls: true, want: "https://pc.mesh.mesh.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{Host: tc.host, URL: &url.URL{Scheme: "https", Host: "attacker.example"}, Header: make(http.Header)}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			r.Header.Set("Forwarded", "host=attacker.example;proto=https")
			r.Header.Set("X-Forwarded-Host", "attacker.example")
			r.Header.Set("X-Forwarded-Proto", "https")
			if got := privateRequestOrigin(r); got != tc.want {
				t.Fatalf("privateRequestOrigin = %q; want %q", got, tc.want)
			}
		})
	}
}
