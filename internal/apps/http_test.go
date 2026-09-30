package apps

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/webauth"
)

func cookieNamed(t *testing.T, response *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatalf("missing cookie %s", name)
	return nil
}
func pairedOwner(t *testing.T, f *appFixture) *http.Cookie {
	t.Helper()
	begin := httptest.NewRecorder()
	f.edge.ServeHost(begin, httptest.NewRequest(http.MethodGet, ManagementOrigin+"/pair", nil), ManagementHost)
	matched := regexp.MustCompile(`Code: <strong>([a-z0-9-]+)</strong>`).FindStringSubmatch(begin.Body.String())
	if len(matched) != 2 {
		t.Fatalf("missing reachable approval code: %s", begin.Body.String())
	}
	if _, err := f.origin.Handle(context.Background(), Request{Action: "browser.approve", Code: matched[1]}); err != nil {
		t.Fatal(err)
	}
	promote := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/", nil)
	request.AddCookie(cookieNamed(t, begin, webauth.PairCookie))
	f.edge.ServeHost(promote, request, ManagementHost)
	if promote.Code != http.StatusSeeOther {
		t.Fatalf("approval did not promote browser: %d %s", promote.Code, promote.Body.String())
	}
	cookie := cookieNamed(t, promote, webauth.OwnerCookie)
	if !cookie.Secure || !cookie.HttpOnly || cookie.Domain != "" || cookie.Path != "/" {
		t.Fatalf("unsafe owner cookie %#v", cookie)
	}
	return cookie
}

func TestPairingPollPreservesCodeAndPromotesAfterApproval(t *testing.T) {
	f := newAppFixture(t)
	begin := httptest.NewRecorder()
	f.edge.ServeHost(begin, httptest.NewRequest(http.MethodGet, ManagementOrigin+"/view?id=7k3d", nil), ManagementHost)
	matched := regexp.MustCompile(`Code: <strong>([a-z0-9-]+)</strong>`).FindStringSubmatch(begin.Body.String())
	if len(matched) != 2 {
		t.Fatal("pairing page is missing its approval code")
	}
	if !strings.Contains(begin.Header().Get("Content-Security-Policy"), "connect-src 'self'") ||
		!strings.Contains(begin.Body.String(), "location.replace(\"/view?id=7k3d\")") {
		t.Fatal("pairing page cannot poll and return to the requested app")
	}
	if !regexp.MustCompile(`performance\.now\(\) \+\s*600000\b`).MatchString(begin.Body.String()) {
		t.Fatal("pairing expiry depends on the browser's wall clock")
	}
	pair := cookieNamed(t, begin, webauth.PairCookie)
	poll := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/pair/status", nil)
		r.AddCookie(cookie)
		response := httptest.NewRecorder()
		f.edge.ServeHost(response, r, ManagementHost)
		return response
	}
	for range 3 {
		pending := poll(pair)
		if pending.Code != http.StatusAccepted || len(pending.Result().Cookies()) != 0 {
			t.Fatalf("checking replaced the pending pairing: %d", pending.Code)
		}
	}
	if _, err := f.origin.Handle(context.Background(), Request{Action: "browser.approve", Code: matched[1]}); err != nil {
		t.Fatalf("original code stopped working after polling: %v", err)
	}
	approved := poll(pair)
	if approved.Code != http.StatusOK || approved.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("approved poll status = %d", approved.Code)
	}
	owner := cookieNamed(t, approved, webauth.OwnerCookie)
	if !owner.Secure || !owner.HttpOnly || owner.Domain != "" {
		t.Fatal("approval poll issued an unsafe cookie")
	}
	request := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/", nil)
	request.AddCookie(owner)
	session, err := f.edge.auth.Browser(context.Background(), request)
	if err != nil || !session.Owns(identityFor(f.ownerKey)) {
		t.Fatalf("poll did not grant the approved owner: %v", err)
	}
	if poll(pair).Code != http.StatusGone {
		t.Fatal("consumed pairing was accepted twice")
	}
	if poll(owner).Code != http.StatusGone {
		t.Fatal("an existing owner session was mistaken for a pending pairing approval")
	}
}

func TestPairingPollRejectsExpiredAndUnboundBrowsers(t *testing.T) {
	f := newAppFixture(t)
	begin := httptest.NewRecorder()
	f.edge.ServeHost(begin, httptest.NewRequest(http.MethodGet, ManagementOrigin+"/pair", nil), ManagementHost)
	pair := cookieNamed(t, begin, webauth.PairCookie)
	f.now = f.now.Add(10 * time.Minute)
	for _, cookie := range []*http.Cookie{nil, pair} {
		request := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/pair/status", nil)
		if cookie != nil {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		f.edge.ServeHost(response, request, ManagementHost)
		if response.Code != http.StatusGone || len(response.Result().Cookies()) != 0 {
			t.Fatalf("invalid pairing poll = %d", response.Code)
		}
	}
	response := httptest.NewRecorder()
	f.edge.ServeHost(response, httptest.NewRequest(http.MethodPost, ManagementOrigin+"/pair/status", nil), ManagementHost)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unsupported poll method = %d", response.Code)
	}
}

func TestPairingAnotherOwnerRequiresItsOwnApproval(t *testing.T) {
	f := newAppFixture(t)
	owner := pairedOwner(t, f)
	request := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/pair", nil)
	request.AddCookie(owner)
	begin := httptest.NewRecorder()
	f.edge.ServeHost(begin, request, ManagementHost)
	pair := cookieNamed(t, begin, webauth.PairCookie)
	poll := func(includePair bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/pair/status", nil)
		r.AddCookie(owner)
		if includePair {
			r.AddCookie(pair)
		}
		response := httptest.NewRecorder()
		f.edge.ServeHost(response, r, ManagementHost)
		return response
	}
	if poll(false).Code != http.StatusGone || poll(true).Code != http.StatusAccepted {
		t.Fatal("existing browser rights bypassed pairing for another owner")
	}
	code := regexp.MustCompile(`Code: <strong>([a-z0-9-]+)</strong>`).FindStringSubmatch(begin.Body.String())
	if len(code) != 2 {
		t.Fatal("missing code for the second owner")
	}
	if err := f.edge.auth.Approve(context.Background(), code[1], identityFor(f.otherKey)); err != nil {
		t.Fatal(err)
	}
	approved := poll(true)
	if approved.Code != http.StatusOK {
		t.Fatalf("second owner approval rejected: %d", approved.Code)
	}
	r := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/", nil)
	r.AddCookie(cookieNamed(t, approved, webauth.OwnerCookie))
	session, err := f.edge.auth.Browser(context.Background(), r)
	if err != nil || !session.Owns(identityFor(f.ownerKey)) || !session.Owns(identityFor(f.otherKey)) {
		t.Fatal("second owner approval did not preserve both explicit grants")
	}
}
func networkOrigin(t *testing.T, f *appFixture) *atomic.Int64 {
	t.Helper()
	count := &atomic.Int64{}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if strings.Contains(r.Header.Get("Cookie"), "__Host-mesh") {
			t.Error("forwarded Mesh cookie to app origin")
		}
		if !f.origin.ServeHTTP(w, r) {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(origin.Close)
	parsed, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := netip.ParseAddrPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	f.edge.config.Resolve = func(context.Context, string) (netip.AddrPort, error) { return endpoint, nil }
	return count
}
func createStaticApp(t *testing.T, f *appFixture) Record {
	t.Helper()
	upload, digest := uploadSource(t, f, sourceFixture(t))
	result, err := f.origin.Handle(context.Background(), Request{Action: "create", Kind: "static", UploadID: upload, Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	return *result.App
}

func TestPillFrameReportsBrowserOwnership(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	if _, err := f.origin.Handle(context.Background(), Request{Action: "public", ID: app.ID}); err != nil {
		t.Fatal(err)
	}
	owner := pairedOwner(t, f)
	check := func(t *testing.T, cookie *http.Cookie, owns string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/frame?id="+app.ID, nil)
		if cookie != nil {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		f.edge.ServeHost(response, request, ManagementHost)
		if response.Code != http.StatusOK || !regexp.MustCompile(`owns:\s*`+owns+`\b`).MatchString(response.Body.String()) {
			t.Fatalf("incorrect pill ownership: %d %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), owner.Value) {
			t.Fatal("pill frame leaked owner credential")
		}
	}
	t.Run("visitor", func(t *testing.T) { check(t, nil, "false") })
	t.Run("owner", func(t *testing.T) { check(t, owner, "true") })
}

func TestPairedBrowserPrivateViewAndTrustedMutation(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	forwarded := networkOrigin(t, f)
	owner := pairedOwner(t, f)
	viewRequest := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/view?id="+app.ID, nil)
	viewRequest.AddCookie(owner)
	viewRedirect := httptest.NewRecorder()
	f.edge.ServeHost(viewRedirect, viewRequest, ManagementHost)
	if viewRedirect.Code != http.StatusSeeOther || !strings.HasPrefix(viewRedirect.Header().Get("Location"), URL(app.ID)+"/?mesh_view=") {
		t.Fatalf("private view grant missing: %d %s", viewRedirect.Code, viewRedirect.Body.String())
	}
	grantURL := viewRedirect.Header().Get("Location")
	consume := httptest.NewRecorder()
	f.edge.ServeHost(consume, httptest.NewRequest(http.MethodGet, grantURL, nil), app.ID+"."+Domain)
	if consume.Code != http.StatusSeeOther || strings.Contains(consume.Header().Get("Location"), "mesh_view") {
		t.Fatal("did not consume and strip private ticket")
	}
	view := cookieNamed(t, consume, webauth.ViewCookie)
	if view.Domain != "" || !view.HttpOnly || !view.Secure {
		t.Fatal("unsafe private-view cookie")
	}
	replay := httptest.NewRecorder()
	f.edge.ServeHost(replay, httptest.NewRequest(http.MethodGet, grantURL, nil), app.ID+"."+Domain)
	if replay.Code != 403 {
		t.Fatalf("ticket replay accepted: %d", replay.Code)
	}
	leakedCookieRequest := httptest.NewRequest(http.MethodGet, URL(app.ID), nil)
	leakedCookieRequest.AddCookie(owner)
	denied := httptest.NewRecorder()
	f.edge.ServeHost(denied, leakedCookieRequest, app.ID+"."+Domain)
	if denied.Code != 303 || forwarded.Load() != 0 {
		t.Fatal("management cookie granted app viewing")
	}
	request := httptest.NewRequest(http.MethodGet, URL(app.ID), nil)
	request.AddCookie(view)
	request.AddCookie(owner)
	result := httptest.NewRecorder()
	f.edge.ServeHost(result, request, app.ID+"."+Domain)
	if result.Code != 200 || !strings.Contains(result.Body.String(), "original page") || !strings.Contains(result.Body.String(), `data-mesh-private="true"`) || !strings.Contains(result.Body.String(), `data-mesh-owns="true"`) {
		t.Fatalf("owner private content missing: %d %s", result.Code, result.Body.String())
	}
	deadline, _, err := f.edge.lookup(context.Background(), app.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	for _, path := range []string{"/.mesh-app/pill.js", "/.mesh-app/pill.css"} {
		request := httptest.NewRequest(http.MethodGet, URL(app.ID)+path, nil)
		request.AddCookie(view)
		asset := httptest.NewRecorder()
		f.edge.ServeHost(asset, request, app.ID+"."+Domain)
		if asset.Code != 200 || asset.Body.Len() == 0 {
			t.Fatalf("owner asset %s unavailable", path)
		}
	}
	unchanged, _, err := f.edge.lookup(context.Background(), app.ID, false)
	if err != nil || !unchanged.ExpiresAt.Equal(deadline.ExpiresAt) {
		t.Fatal("control assets renewed lifetime")
	}
	authRequest := httptest.NewRequest(http.MethodGet, ManagementOrigin, nil)
	authRequest.AddCookie(owner)
	session, err := f.edge.auth.Browser(context.Background(), authRequest)
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{URL(app.ID), "https://other.shaulavo.dev"} {
		form := url.Values{"id": {app.ID}, "action": {"public"}, "csrf": {session.CSRF}}
		forged := httptest.NewRequest(http.MethodPost, ManagementOrigin+"/action", strings.NewReader(form.Encode()))
		forged.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		forged.Header.Set("Origin", origin)
		forged.AddCookie(owner)
		response := httptest.NewRecorder()
		f.edge.ServeHost(response, forged, ManagementHost)
		if response.Code != 403 {
			t.Fatalf("sibling-origin mutation status %d", response.Code)
		}
	}
	form := url.Values{"id": {app.ID}, "action": {"public"}, "csrf": {session.CSRF}}
	mutation := httptest.NewRequest(http.MethodPost, ManagementOrigin+"/action", strings.NewReader(form.Encode()))
	mutation.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mutation.Header.Set("Origin", ManagementOrigin)
	mutation.AddCookie(owner)
	changed := httptest.NewRecorder()
	f.edge.ServeHost(changed, mutation, ManagementHost)
	if changed.Code != 303 {
		t.Fatalf("owner mutation rejected: %d %s", changed.Code, changed.Body.String())
	}
	actual, _, err := f.edge.lookup(context.Background(), app.ID, false)
	if err != nil || actual.Visibility != "public" {
		t.Fatal("trusted owner did not gain authority")
	}
}

func TestPublicRequestBlockedByPrivacyChangeDuringOriginResolution(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	forwarded := networkOrigin(t, f)
	if _, err := f.origin.Handle(context.Background(), Request{Action: "public", ID: app.ID}); err != nil {
		t.Fatal(err)
	}
	resolve := f.edge.config.Resolve
	entered := make(chan struct{})
	release := make(chan struct{})
	f.edge.config.Resolve = func(ctx context.Context, owner string) (netip.AddrPort, error) {
		close(entered)
		select {
		case <-release:
			return resolve(ctx, owner)
		case <-ctx.Done():
			return netip.AddrPort{}, ctx.Err()
		}
	}
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		f.edge.ServeHost(response, httptest.NewRequest(http.MethodGet, URL(app.ID), nil), app.ID+"."+Domain)
		finished <- response
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not reach resolve")
	}
	if _, err := f.origin.Handle(context.Background(), Request{Action: "private", ID: app.ID}); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	select {
	case response := <-finished:
		if response.Code != 403 || forwarded.Load() != 0 {
			t.Fatalf("stale public request forwarded: status %d, count %d", response.Code, forwarded.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("request stuck after privacy change")
	}
}

func TestVisibilityConfirmationPreservesBrowserOriginAndReturn(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	cookie := pairedOwner(t, f)
	destination := URL(app.ID) + "/colors?palette=rose%20water#favorites"
	request := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/confirm?id="+app.ID+"&action=public&return="+url.QueryEscape(destination), nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	f.edge.ServeHost(response, request, ManagementHost)
	if response.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatal("confirmation suppresses the Origin header needed by its own POST")
	}
	if !strings.Contains(response.Header().Get("Content-Security-Policy"), "form-action 'self' "+URL(app.ID)+";") {
		t.Fatal("confirmation blocks its form redirect back to the app")
	}
	if !strings.Contains(response.Body.String(), `name="return"`) {
		t.Fatal("confirmation lost the originating app page")
	}
	session, err := f.edge.auth.Browser(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"id": {app.ID}, "action": {"public"}, "csrf": {session.CSRF}, "return": {destination}}
	post := httptest.NewRequest(http.MethodPost, ManagementOrigin+"/action", strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.Header.Set("Origin", ManagementOrigin)
	post.AddCookie(cookie)
	changed := httptest.NewRecorder()
	f.edge.ServeHost(changed, post, ManagementHost)
	view := httptest.NewRequest(http.MethodGet, ManagementOrigin+changed.Header().Get("Location"), nil)
	view.AddCookie(cookie)
	back := httptest.NewRecorder()
	f.edge.ServeHost(back, view, ManagementHost)
	if back.Header().Get("Location") != destination {
		t.Fatalf("returned to %q, want %q", back.Header().Get("Location"), destination)
	}
}

func TestPrivateViewReturnPreservesPageAcrossTicketExchange(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	cookie := pairedOwner(t, f)
	destination := URL(app.ID) + "/colors?palette=rose%20water#favorites"
	request := httptest.NewRequest(http.MethodGet, ManagementOrigin+"/view?id="+app.ID+"&return="+url.QueryEscape(destination), nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	f.edge.ServeHost(response, request, ManagementHost)
	target, err := url.Parse(response.Header().Get("Location"))
	if err != nil || target.Path != "/colors" || target.Fragment != "favorites" || target.Query().Get("palette") != "rose water" || target.Query().Get("mesh_view") == "" {
		t.Fatalf("private view lost its destination: %s", target)
	}
	consume := httptest.NewRecorder()
	wireTarget := *target
	wireTarget.Fragment = ""
	wireTarget.RawFragment = ""
	f.edge.ServeHost(consume, httptest.NewRequest(http.MethodGet, wireTarget.String(), nil), app.ID+"."+Domain)
	clean, err := url.Parse(consume.Header().Get("Location"))
	if err != nil || clean.Path != "/colors" || clean.Query().Get("palette") != "rose water" || clean.Query().Get("mesh_view") != "" {
		t.Fatalf("ticket cleanup lost its app page: %s", clean)
	}
}

func TestAppReturnRejectsOtherOrigins(t *testing.T) {
	fallback := URL("7k3d") + "/"
	for _, value := range []string{"https://evil.test/", "//evil.test/", "https://7k3d.shaulavo.dev@evil.test/", "https://7k3d.shaulavo.dev.evil.test/", "https://other.shaulavo.dev/", "http://7k3d.shaulavo.dev/", "https://7k3d.shaulavo.dev\\evil.test/", "https://7k3d.shaulavo.dev/\r\nLocation: https://evil.test"} {
		if got := appReturn("7k3d", value); got != fallback {
			t.Fatalf("accepted outside return %q as %q", value, got)
		}
	}
}
