package edge

import (
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/serve"
)

func FuzzCanonicalPublicHost(f *testing.F) {
	for _, seed := range []string{
		"app.shaulavo.dev", "app.shaulavo.dev:1", "app.shaulavo.dev:80", "app.shaulavo.dev:443", "app.shaulavo.dev:65535",
		strings.Repeat("a", 63) + ".shaulavo.dev", ":443", "[app.shaulavo.dev]:443", "APP.shaulavo.dev", "app.shaulavo.dev.",
		"shaulavo.dev", "a.b.shaulavo.dev", "mesh.shaulavo.dev", "*.shaulavo.dev", "-app.shaulavo.dev", "app-.shaulavo.dev",
		"app_name.shaulavo.dev", "café.shaulavo.dev", "app.shaulavo.dev:", "app.shaulavo.dev:0", "app.shaulavo.dev:00",
		"app.shaulavo.dev:+1", "app.shaulavo.dev:-1", "app.shaulavo.dev:65536", "app.shaulavo.dev:99999999999999999999",
		"app.shaulavo.dev:http", "app.shaulavo.dev: 443", "192.0.2.1", "[2001:db8::1]:443", "user@app.shaulavo.dev",
		"app.shaulavo.dev/path", "app.shaulavo.dev\\path", "app.shaulavo.dev\r\nInjected: true", "app.shaulavo.dev\x00",
		"app.shaulavo.dev,other.shaulavo.dev", string([]byte{0xff, '.', 's', 'h', 'a', 'u', 'l', 'a', 'v', 'o', '.', 'd', 'e', 'v'}),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		host, forwarded, err := canonicalPublicHost(value)
		if err != nil {
			return
		}
		if host == "" {
			t.Fatal("successful public host is empty")
		}
		if err := serve.ValidatePublicName(host); err != nil {
			t.Fatalf("successful host %q is invalid: %v", host, err)
		}
		if forwarded != value {
			t.Fatalf("successful host rewrote %q to %q", value, forwarded)
		}
		if !strings.Contains(value, ":") {
			if host != value {
				t.Fatalf("bare host = %q, want input %q", host, value)
			}
		} else {
			parsedHost, port, err := net.SplitHostPort(value)
			if err != nil {
				t.Fatalf("successful host does not split: %v", err)
			}
			if parsedHost != host || net.JoinHostPort(parsedHost, port) != value {
				t.Fatalf("host/port did not round-trip: host %q, port %q", parsedHost, port)
			}
			parsedPort, err := strconv.ParseUint(port, 10, 16)
			if err != nil || parsedPort == 0 || strconv.FormatUint(parsedPort, 10) != port {
				t.Fatalf("successful port %q is not canonical", port)
			}
		}
		hostAgain, forwardedAgain, err := canonicalPublicHost(forwarded)
		if err != nil || hostAgain != host || forwardedAgain != forwarded {
			t.Fatalf("second parse = %q, %q, %v; want %q, %q", hostAgain, forwardedAgain, err, host, forwarded)
		}
	})
}
