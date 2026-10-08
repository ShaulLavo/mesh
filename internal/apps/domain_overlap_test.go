package apps

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAppAliasesRetainOwnersAndRequestHost(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	networkOrigin(t, f)
	authenticate := ambientOwnerRequest(t, f, app, "network")
	for _, domain := range []string{"mesh.test", "old.test"} {
		name := app.ID + "." + domain
		if exists, err := f.edgeStore.AppNameExists(context.Background(), name); err != nil || !exists {
			t.Fatalf("alias reservation %s: exists=%v err=%v", name, exists, err)
		}
		request := httptest.NewRequest(http.MethodGet, "https://"+name+"/index.html", nil)
		authenticate(request)
		response := httptest.NewRecorder()
		if !f.edge.ServeHost(response, request, name) {
			t.Fatalf("alias not dispatched: %s", name)
		}
		if response.Code != http.StatusOK {
			t.Fatalf("alias status %s: %d %s", name, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "apps.mesh.test") && domain == "old.test" {
			t.Fatal("alias pill points at other manager origin")
		}
	}
	request := httptest.NewRequest(http.MethodGet, "https://"+app.ID+".other.test/", nil)
	authenticate(request)
	if f.edge.ServeHost(httptest.NewRecorder(), request, app.ID+".other.test") {
		t.Fatal("unconfigured app hostname accepted")
	}
}

func TestAppReturnAcceptsOnlyItsConfiguredAliases(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, ManagementOrigin(), nil)
	id := "7k3d"
	for _, domain := range []string{"mesh.test", "old.test"} {
		target := "https://" + id + "." + domain + "/page?x=1"
		if appReturn(request, id, target) != target {
			t.Fatalf("alias return rejected: %s", target)
		}
	}
	for _, target := range []string{"https://7k3d.other.test/", "https://zzzz.old.test/", "https://7k3d.old.test.evil/", "https://7k3d.old.test:443/"} {
		if appReturn(request, id, target) != URL(id)+"/" {
			t.Fatalf("unsafe return accepted: %s", target)
		}
	}
}

func TestAliasManagerOriginStaysSameOrigin(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://apps.old.test/pair", nil)
	request.Header.Set("Origin", "https://apps.old.test")
	if managementOrigin(request) != "https://apps.old.test" || !downloadAllowed(request) {
		t.Fatal("alias manager origin rejected")
	}
	request.Header.Set("Origin", ManagementOrigin())
	if downloadAllowed(request) {
		t.Fatal("cross-domain manager Origin accepted")
	}
}

func TestAliasManagerViewKeepsAppDomain(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	networkOrigin(t, f)
	authenticate := ambientOwnerRequest(t, f, app, "network")
	request := httptest.NewRequest(http.MethodGet, "https://apps.old.test/view?id="+app.ID, nil)
	authenticate(request)
	response := httptest.NewRecorder()
	if !f.edge.ServeHost(response, request, "apps.old.test") {
		t.Fatal("alias manager not dispatched")
	}
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "https://"+app.ID+".old.test/" {
		t.Fatalf("alias view redirect: %d %s", response.Code, response.Header().Get("Location"))
	}
}

func TestAppReturnKeepsNormalizedAliasAndRequestFallback(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://apps.mesh.test/view", nil)
	if got := appReturn(request, "7k3d", "https://7k3d.old.test"); got != "https://7k3d.old.test/" {
		t.Fatalf("normalized alias return: %s", got)
	}
	alias := httptest.NewRequest(http.MethodGet, "https://apps.old.test/view", nil)
	if got := appReturn(alias, "7k3d", "https://evil.test/"); got != "https://7k3d.old.test/" {
		t.Fatalf("request-domain fallback: %s", got)
	}
}

func TestAliasManagerListAndFrameKeepRequestDomain(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	networkOrigin(t, f)
	authenticate := ambientOwnerRequest(t, f, app, "network")
	for _, path := range []string{"/", "/frame?id=" + app.ID} {
		request := httptest.NewRequest(http.MethodGet, "https://apps.old.test"+path, nil)
		authenticate(request)
		response := httptest.NewRecorder()
		f.edge.ServeHost(response, request, "apps.old.test")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "https://"+app.ID+".old.test") || strings.Contains(response.Body.String(), "https://"+app.ID+".mesh.test") {
			t.Fatalf("alias manager page %s: %d %s", path, response.Code, response.Body.String())
		}
		if path != "/" && !strings.Contains(response.Header().Get("Content-Security-Policy"), "frame-ancestors https://"+app.ID+".old.test") {
			t.Fatal("alias frame policy points at another domain")
		}
	}
}

func TestPrivateAliasReturnConsumesTicketWithHostOnlyCookies(t *testing.T) {
	for _, domain := range []string{"mesh.test", "old.test"} {
		t.Run(domain, func(t *testing.T) {
			f := newAppFixture(t)
			app := createStaticApp(t, f)
			owner := pairedOwner(t, f)
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			manager, _ := url.Parse(ManagementOrigin())
			jar.SetCookies(manager, []*http.Cookie{owner})
			destination := "https://" + app.ID + "." + domain + "/page?x=1"
			current := ManagementOrigin() + "/view?" + url.Values{"id": {app.ID}, "return": {destination}}.Encode()
			for step := 0; step < 4; step++ {
				request := httptest.NewRequest(http.MethodGet, current, nil)
				for _, cookie := range jar.Cookies(request.URL) {
					request.AddCookie(cookie)
				}
				response := httptest.NewRecorder()
				if !f.edge.ServeHost(response, request, request.URL.Hostname()) {
					t.Fatal("redirect left deployment")
				}
				if response.Code != http.StatusSeeOther {
					t.Fatalf("step %d %s: %d %s", step, current, response.Code, response.Body.String())
				}
				for _, cookie := range response.Result().Cookies() {
					if cookie.Domain != "" {
						t.Fatal("view flow widened a cookie's domain")
					}
				}
				jar.SetCookies(request.URL, response.Result().Cookies())
				current = response.Header().Get("Location")
			}
			if current != destination {
				t.Fatalf("consumed return: %s", current)
			}
		})
	}
}
