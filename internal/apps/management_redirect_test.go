package apps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/webauth"
)

func TestManagementReturnReconstructsAllowedPaths(t *testing.T) {
	for _, tc := range []struct {
		name, path, opaque, query, wantPath string
	}{
		{name: "root", path: "/", wantPath: "/"},
		{name: "view", path: "/view", query: "id=7k3d&return=https%3A%2F%2F7k3d.mesh.test%2F", wantPath: "/view"},
		{name: "confirm", path: "/confirm", query: "action=delete&id=7k3d", wantPath: "/confirm"},
		{name: "encoded query", path: "/view", query: "id=7k3d&x=%2F%2Fattacker.example&x=a%26b%3Dc&y=%0D%0A", wantPath: "/view"},
		{name: "opaque representation", path: "/view", opaque: "//attacker.example", query: "id=7k3d", wantPath: "/view"},
		{name: "scheme relative path", path: "//attacker.example", wantPath: "/"},
		{name: "other management path", path: "/pair", wantPath: "/"},
		{name: "oversized query", path: "/view", query: strings.Repeat("x", 4097), wantPath: "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, ManagementOrigin()+"/", nil)
			r.URL.Path, r.URL.Opaque, r.URL.RawQuery = tc.path, tc.opaque, tc.query
			got, err := url.Parse(managementReturn(r))
			if err != nil || got.IsAbs() || got.Host != "" || got.Opaque != "" || got.Path != tc.wantPath {
				t.Fatalf("management return = %v, %v; want relative %s", got, err, tc.wantPath)
			}
			if tc.path == tc.wantPath && len(tc.query) <= 4096 && !reflect.DeepEqual(got.Query(), r.URL.Query()) {
				t.Fatalf("management query changed: %v; want %v", got.Query(), r.URL.Query())
			}
		})
	}
}

func TestPairingPromotionRedirectUsesCanonicalManagementOrigin(t *testing.T) {
	f := newAppFixture(t)
	begin := httptest.NewRecorder()
	f.edge.ServeHost(begin, pairingStartRequest("/"), ManagementHost())
	code := regexp.MustCompile(`Code: <strong>([a-z0-9-]+)</strong>`).FindStringSubmatch(begin.Body.String())
	if len(code) != 2 {
		t.Fatal("missing pairing code")
	}
	if _, err := f.origin.Handle(context.Background(), Request{Action: "browser.approve", Code: code[1]}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "https://attacker.example/view?id=7k3d&return=%2F%2Fattacker.example", nil)
	r.AddCookie(cookieNamed(t, begin, webauth.PairCookie))
	promote := httptest.NewRecorder()
	f.edge.ServeHost(promote, r, ManagementHost())
	target, err := url.Parse(promote.Header().Get("Location"))
	if err != nil || promote.Code != http.StatusSeeOther || target.Scheme != "https" || target.Host != ManagementHost() || target.Path != "/view" || !reflect.DeepEqual(target.Query(), r.URL.Query()) {
		t.Fatalf("pairing redirect = %d %v, %v", promote.Code, target, err)
	}
}
