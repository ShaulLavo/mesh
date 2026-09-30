package apps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestTailnetOwnerCanViewAndToggleWithoutPairing(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	_ = networkOrigin(t, f)
	ownerIP := netip.MustParseAddr("100.64.0.2")
	recognize := true
	f.edge.config.ClientIP = func(r *http.Request) netip.Addr {
		address, _ := netip.ParseAddrPort(r.RemoteAddr)
		return address.Addr()
	}
	f.edge.config.NetworkOwners = func(_ context.Context, ip netip.Addr) ([]string, error) {
		if recognize && ip == ownerIP {
			return []string{app.Owner}, nil
		}
		return nil, nil
	}
	request := func(path, ip string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = ip + ":12345"
		return r
	}
	view := httptest.NewRecorder()
	f.edge.ServeHost(view, request(URL(app.ID)+"/", ownerIP.String()), app.ID+"."+Domain)
	if view.Code != http.StatusOK || len(view.Result().Cookies()) != 0 {
		t.Fatalf("Tailnet owner cannot view privately without cookies: %d", view.Code)
	}
	for _, visibility := range []string{"private", "public"} {
		frame := httptest.NewRecorder()
		f.edge.ServeHost(frame, request(ManagementOrigin+"/frame?id="+app.ID, ownerIP.String()), ManagementHost)
		if !regexp.MustCompile(`owns:\s*true\b`).MatchString(frame.Body.String()) {
			t.Fatal("fresh owner browser is missing controls")
		}
		action := "public"
		if visibility == "public" {
			action = "private"
		}
		confirm := httptest.NewRecorder()
		f.edge.ServeHost(confirm, request(ManagementOrigin+"/confirm?id="+app.ID+"&action="+action, ownerIP.String()), ManagementHost)
		csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(confirm.Body.String())
		if len(csrf) != 2 {
			t.Fatal("owner was asked to pair instead of confirming")
		}
		form := url.Values{"id": {app.ID}, "action": {action}, "csrf": {csrf[1]}}
		post := func(ip, origin string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodPost, ManagementOrigin+"/action", strings.NewReader(form.Encode()))
			r.RemoteAddr = ip + ":12345"
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", origin)
			r.Header.Set("X-Forwarded-For", ownerIP.String())
			w := httptest.NewRecorder()
			f.edge.ServeHost(w, r, ManagementHost)
			return w
		}
		if post("203.0.113.1", ManagementOrigin).Code < 400 || post("100.64.0.3", ManagementOrigin).Code < 400 || post(ownerIP.String(), "https://attacker.test").Code < 400 {
			t.Fatal("visitor, spoofed IP, or cross-origin page acquired owner authority")
		}
		if got := post(ownerIP.String(), ManagementOrigin); got.Code != http.StatusSeeOther {
			t.Fatalf("owner toggle failed: %d %s", got.Code, got.Body.String())
		}
		if action == "public" {
			visitor := httptest.NewRecorder()
			f.edge.ServeHost(visitor, request(ManagementOrigin+"/frame?id="+app.ID, "203.0.113.1"), ManagementHost)
			if !regexp.MustCompile(`owns:\s*false\b`).MatchString(visitor.Body.String()) {
				t.Fatal("public visitor was given owner controls")
			}
		}
	}
	privateVisitor := httptest.NewRecorder()
	f.edge.ServeHost(privateVisitor, request(URL(app.ID)+"/", "100.64.0.3"), app.ID+"."+Domain)
	if privateVisitor.Code != http.StatusSeeOther {
		t.Fatal("unrecognized device viewed private app")
	}
	recognize = false
	lost := httptest.NewRecorder()
	f.edge.ServeHost(lost, request(URL(app.ID)+"/", ownerIP.String()), app.ID+"."+Domain)
	if lost.Code != http.StatusSeeOther {
		t.Fatal("automatic authority survived losing Tailnet identity")
	}
	browsers, err := f.edge.auth.List(context.Background(), app.Owner)
	if err != nil || len(browsers) != 0 {
		t.Fatal("automatic access created a persistent browser grant")
	}

}
