//go:build !linux

package edge

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUnverifiableForwarderDoesNotTrustXFF(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	if trustedLoopbackForwarder(r) {
		t.Fatal("unverifiable forwarded identity trusted")
	}
}
