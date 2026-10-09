package apps

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyCookieIsolationPreservesApplicationCookies(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, URL("7k3d"), nil)
	request.Header.Add("Cookie", "__Host-mesh-view=secret; app=session")
	request.Header.Add("Cookie", "__Host-mesh-session=owner; __Secure-MESH-session=owner")
	stripRequestCookies(request)
	if got := request.Header.Get("Cookie"); got != "app=session" {
		t.Fatalf("upstream cookies=%q", got)
	}
	header := make(http.Header)
	header.Add("Set-Cookie", "__Host-mesh-view=forged; Secure; Path=/")
	header.Add("Set-Cookie", "__Secure-MESH-session=forged; Secure")
	header.Add("Set-Cookie", "app=session; Secure")
	stripCookies(header)
	if got := header.Values("Set-Cookie"); len(got) != 1 || got[0] != "app=session; Secure" {
		t.Fatalf("downstream cookies=%q", got)
	}
}
