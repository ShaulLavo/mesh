package apps

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/shaul/mesh/internal/serve"
)

func TestAmbientOwnerAllowed(t *testing.T) {
	const origin = "https://7k3d.shaulavo.dev"
	for _, tc := range []struct {
		name, method, site, mode, dest, origin, upgrade string
		want                                            bool
	}{
		{name: "same-origin fetch", method: "GET", site: "same-origin", mode: "cors", dest: "empty", want: true},
		{name: "same-origin POST", method: "POST", site: "same-origin", origin: origin, want: true},
		{name: "direct navigation", method: "GET", site: "none", want: true},
		{name: "cross-site top-level GET", method: "GET", site: "cross-site", mode: "navigate", dest: "document", want: true},
		{name: "cross-site top-level HEAD", method: "HEAD", site: "cross-site", mode: "navigate", dest: "document", want: true},
		{name: "same-site top-level GET", method: "GET", site: "same-site", mode: "navigate", dest: "document", want: true},
		{name: "cross-site iframe", method: "GET", site: "cross-site", mode: "navigate", dest: "iframe"},
		{name: "cross-site form POST", method: "POST", site: "cross-site", mode: "navigate", dest: "document", origin: "https://attacker.example"},
		{name: "cross-site image", method: "GET", site: "cross-site", mode: "no-cors", dest: "image"},
		{name: "same-site fetch", method: "GET", site: "same-site", mode: "cors", dest: "empty"},
		{name: "unknown site fetch", method: "GET", site: "invalid", mode: "cors", dest: "empty"},
		{name: "legacy foreign GET", method: "GET", origin: "https://attacker.example"},
		{name: "legacy foreign HEAD", method: "HEAD", origin: "https://attacker.example"},
		{name: "legacy foreign OPTIONS", method: "OPTIONS", origin: "https://attacker.example"},
		{name: "legacy foreign TRACE", method: "TRACE", origin: "https://attacker.example"},
		{name: "same-origin metadata with foreign Origin", method: "GET", site: "same-origin", origin: "https://attacker.example"},
		{name: "navigation metadata with foreign Origin", method: "GET", site: "cross-site", mode: "navigate", dest: "document", origin: "https://attacker.example"},
		{name: "direct metadata with foreign Origin", method: "GET", site: "none", origin: "https://attacker.example"},
		{name: "legacy same-origin GET", method: "GET", origin: origin, want: true},
		{name: "legacy HEAD", method: "HEAD", want: true},
		{name: "legacy OPTIONS", method: "OPTIONS", want: true},
		{name: "legacy TRACE", method: "TRACE", want: true},
		{name: "curl POST", method: "POST", want: true},
		{name: "legacy same-origin POST", method: "POST", origin: origin, want: true},
		{name: "legacy foreign POST", method: "POST", origin: "https://attacker.example"},
		{name: "legacy sibling PUT", method: "PUT", origin: "https://zzzz.shaulavo.dev"},
		{name: "legacy null origin", method: "DELETE", origin: "null"},
		{name: "same-origin no-referrer form", method: "POST", site: "same-origin", mode: "navigate", dest: "document", origin: "null", want: true},
		{name: "cross-site null origin", method: "POST", site: "cross-site", mode: "navigate", dest: "document", origin: "null"},
		{name: "same-site null origin", method: "POST", site: "same-site", mode: "navigate", dest: "document", origin: "null"},
		{name: "null origin without fetch metadata", method: "POST", origin: "null"},
		{name: "same-origin null websocket", method: "GET", site: "same-origin", origin: "null", upgrade: "websocket"},
		{name: "legacy origin prefix", method: "PATCH", origin: origin + ".attacker.example"},
		{name: "same-origin websocket", method: "GET", site: "same-origin", origin: origin, upgrade: "websocket", want: true},
		{name: "legacy same-origin websocket", method: "GET", origin: origin, upgrade: "WebSocket", want: true},
		{name: "cross-origin websocket", method: "GET", site: "same-origin", origin: "https://attacker.example", upgrade: "websocket"},
		{name: "websocket missing origin", method: "GET", site: "same-origin", upgrade: "websocket"},
		{name: "navigation cannot bypass websocket origin", method: "GET", site: "cross-site", mode: "navigate", dest: "document", origin: "https://attacker.example", upgrade: "websocket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, origin+"/", nil)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			r.Header.Set("Sec-Fetch-Mode", tc.mode)
			r.Header.Set("Sec-Fetch-Dest", tc.dest)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Upgrade", tc.upgrade)
			if got := serve.AmbientOwnerAllowed(r, origin, serve.RequireWebSocketOrigin); got != tc.want {
				t.Fatalf("AmbientOwnerAllowed = %v; want %v", got, tc.want)
			}
		})
	}
}

func ambientAppBackend(t *testing.T, f *appFixture, id string, handler http.HandlerFunc) {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)
	local := f.origin.state.Apps[id]
	local.Record.Kind = "server"
	local.Command = "fixture"
	local.Port = backend.Listener.Addr().(*net.TCPAddr).Port
	f.origin.state.Apps[id] = local
	f.origin.publishRoutes()
}

func TestPrivateAppAllowsIntentionalOwnerRequests(t *testing.T) {
	for _, credential := range []string{"network", "view cookie"} {
		t.Run(credential, func(t *testing.T) {
			f := newAppFixture(t)
			app := createStaticApp(t, f)
			forwarded := networkOrigin(t, f)
			authenticate := ambientOwnerRequest(t, f, app, credential)
			seen := make(chan string, 1)
			ambientAppBackend(t, f, app.ID, func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				seen <- r.Method + ":" + string(body)
				w.Header().Set("Content-Type", "text/html")
				w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
				w.Header().Set("X-Frame-Options", "ALLOWALL")
				_, _ = io.WriteString(w, "<html><body>owner app</body></html>")
			})
			for _, tc := range []struct{ name, method, site, mode, dest, origin string }{
				{"same-origin fetch", "GET", "same-origin", "cors", "empty", URL(app.ID)},
				{"same-origin POST", "POST", "same-origin", "cors", "empty", URL(app.ID)},
				{"same-origin no-referrer form", "POST", "same-origin", "navigate", "document", "null"},
				{"cross-site top-level", "GET", "cross-site", "navigate", "document", ""},
				{"same-site top-level", "HEAD", "same-site", "navigate", "document", ""},
				{"direct navigation", "GET", "none", "navigate", "document", ""},
				{"curl GET", "GET", "", "", "", ""},
				{"curl POST", "POST", "", "", "", ""},
				{"legacy same-origin POST", "POST", "", "", "", URL(app.ID)},
			} {
				t.Run(tc.name, func(t *testing.T) {
					before := forwarded.Load()
					r := httptest.NewRequest(tc.method, URL(app.ID)+"/api", strings.NewReader("confirm=yes"))
					authenticate(r)
					r.Header.Set("Sec-Fetch-Site", tc.site)
					r.Header.Set("Sec-Fetch-Mode", tc.mode)
					r.Header.Set("Sec-Fetch-Dest", tc.dest)
					r.Header.Set("Origin", tc.origin)
					w := httptest.NewRecorder()
					f.edge.ServeHost(w, r, app.ID+"."+Domain)
					if w.Code != http.StatusOK || forwarded.Load() != before+1 {
						t.Fatalf("owner request status=%d, upstream hits=%d; want 200 and one", w.Code, forwarded.Load()-before)
					}
					select {
					case body := <-seen:
						if body != tc.method+":confirm=yes" {
							t.Fatalf("upstream method/body = %q", body)
						}
					default:
						t.Fatal("owner request did not reach app process")
					}
					for key, want := range map[string]string{"Cross-Origin-Resource-Policy": "same-origin", "X-Frame-Options": "SAMEORIGIN"} {
						if got := w.Header().Values(key); len(got) != 1 || got[0] != want {
							t.Errorf("%s = %q; want one %q policy", key, got, want)
						}
					}
					if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'self'") {
						t.Fatal("private app has no framing restriction")
					}
				})
			}
		})
	}
}

func TestPrivateAppWebSocketRequiresOwnOrigin(t *testing.T) {
	for _, credential := range []string{"network", "view cookie"} {
		t.Run(credential, func(t *testing.T) {
			f := newAppFixture(t)
			app := createStaticApp(t, f)
			forwarded := networkOrigin(t, f)
			authenticate := ambientOwnerRequest(t, f, app, credential)
			var messages atomic.Int64
			ambientAppBackend(t, f, app.ID, func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.CloseNow() }()
				kind, message, err := conn.Read(r.Context())
				if err != nil {
					t.Error(err)
					return
				}
				messages.Add(1)
				if err := conn.Write(r.Context(), kind, message); err != nil {
					t.Error(err)
				}
			})
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if credential == "network" {
					r.RemoteAddr = "100.64.0.2:4444"
				}
				f.edge.ServeHost(w, r, app.ID+"."+Domain)
			}))
			defer front.Close()
			for _, origin := range []string{URL(app.ID), "https://zzzz.shaulavo.dev", "https://attacker.example", "null", ""} {
				r := httptest.NewRequest(http.MethodGet, URL(app.ID)+"/socket", nil)
				authenticate(r)
				r.Header.Set("Origin", origin)
				r.Header.Set("Sec-Fetch-Site", map[string]string{
					URL(app.ID):                 "same-origin",
					"https://zzzz.shaulavo.dev": "same-site",
					"https://attacker.example":  "cross-site",
					"null":                      "same-origin",
					"":                          "same-origin",
				}[origin])
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				before := forwarded.Load()
				conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http")+"/socket", &websocket.DialOptions{HTTPHeader: r.Header})
				if response != nil && response.Body != nil {
					defer func() { _ = response.Body.Close() }()
				}
				if origin != URL(app.ID) {
					cancel()
					if err == nil {
						_ = conn.CloseNow()
						t.Fatalf("websocket from %q was accepted", origin)
					}
					if response == nil || response.StatusCode != http.StatusForbidden || forwarded.Load() != before {
						t.Fatalf("websocket from %q was not blocked before upstream: %v, response=%v", origin, err, response)
					}
					continue
				}
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				if err := conn.Write(ctx, websocket.MessageText, []byte("owner message")); err != nil {
					t.Error(err)
				}
				_, message, err := conn.Read(ctx)
				_ = conn.CloseNow()
				cancel()
				if err != nil || string(message) != "owner message" || messages.Load() != 1 {
					t.Fatalf("owner websocket did not echo app data: %q, %v", message, err)
				}
			}
		})
	}
}

func TestPublicAppDoesNotApplyAmbientOwnerGate(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	forwarded := networkOrigin(t, f)
	if _, err := f.origin.Handle(context.Background(), Request{Action: "public", ID: app.ID}); err != nil {
		t.Fatal(err)
	}
	ambientAppBackend(t, f, app.ID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
		w.Header().Set("Content-Security-Policy", "frame-ancestors *")
		_, _ = io.WriteString(w, "<html><body>public app</body></html>")
	})
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		r := httptest.NewRequest(method, URL(app.ID)+"/", nil)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		r.Header.Set("Origin", "https://attacker.example")
		w := httptest.NewRecorder()
		f.edge.ServeHost(w, r, app.ID+"."+Domain)
		if w.Code != http.StatusOK || w.Header().Get("Cross-Origin-Resource-Policy") != "cross-origin" || w.Header().Get("X-Frame-Options") != "" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors *") || strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'self'") {
			t.Fatalf("public app behavior changed: %d %v", w.Code, w.Header())
		}
	}
	if forwarded.Load() != 2 {
		t.Fatalf("public requests upstream hits=%d; want two", forwarded.Load())
	}
}

func TestPrivateAppPreservesStricterFramingOnWire(t *testing.T) {
	for _, kind := range []string{"text/html", "application/json"} {
		t.Run(kind, func(t *testing.T) {
			f := newAppFixture(t)
			app := createStaticApp(t, f)
			_ = networkOrigin(t, f)
			authenticate := ambientOwnerRequest(t, f, app, "network")
			ambientAppBackend(t, f, app.ID, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", kind)
				w.Header().Set("X-Frame-Options", "DENY")
				w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
				w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
				_, _ = io.WriteString(w, "<html><body>private app</body></html>")
			})
			r := httptest.NewRequest(http.MethodGet, URL(app.ID)+"/", nil)
			authenticate(r)
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			w := httptest.NewRecorder()
			f.edge.ServeHost(w, r, app.ID+"."+Domain)
			if w.Code != http.StatusOK || w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" {
				t.Fatalf("private response policies = %d %v", w.Code, w.Header())
			}
			policies := w.Header().Values("Content-Security-Policy")
			if len(policies) != 2 || !strings.Contains(policies[0], "frame-ancestors 'none'") || !strings.Contains(policies[1], "frame-ancestors 'self'") {
				t.Fatalf("private CSP intersection = %q", policies)
			}
		})
	}
}

func TestPrivateAppGateRechecksVisibilityAtAdmission(t *testing.T) {
	f := newAppFixture(t)
	app := createStaticApp(t, f)
	forwarded := networkOrigin(t, f)
	authenticate := ambientOwnerRequest(t, f, app, "network")
	if _, err := f.origin.Handle(context.Background(), Request{Action: "public", ID: app.ID}); err != nil {
		t.Fatal(err)
	}
	resolve := f.edge.config.Resolve
	f.edge.config.Resolve = func(ctx context.Context, owner string) (netip.AddrPort, error) {
		if _, err := f.edge.apply(ctx, app.Owner, Request{Action: "private", ID: app.ID}); err != nil {
			return netip.AddrPort{}, err
		}
		return resolve(ctx, owner)
	}
	r := httptest.NewRequest(http.MethodPost, URL(app.ID)+"/api", nil)
	authenticate(r)
	r.Header.Set("Sec-Fetch-Site", "same-site")
	r.Header.Set("Origin", "https://zzzz.shaulavo.dev")
	w := httptest.NewRecorder()
	f.edge.ServeHost(w, r, app.ID+"."+Domain)
	if w.Code != http.StatusForbidden || forwarded.Load() != 0 {
		t.Fatalf("visibility race bypassed private gate: status=%d, upstream hits=%d", w.Code, forwarded.Load())
	}
}

func TestPublicCrossPageOwnerRequestIsRevokedWhenMadePrivate(t *testing.T) {
	for _, credential := range []string{"network", "view cookie"} {
		for _, site := range []string{"same-site", "same-origin"} {
			t.Run(credential+"/"+site, func(t *testing.T) {
				f := newAppFixture(t)
				app := createStaticApp(t, f)
				authenticate := ambientOwnerRequest(t, f, app, credential)
				if _, err := f.edge.apply(context.Background(), app.Owner, Request{Action: "public", ID: app.ID}); err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest(http.MethodGet, URL(app.ID)+"/stream", nil)
				authenticate(r)
				r.Header.Set("Sec-Fetch-Site", site)
				_, admitted, release, err := f.edge.admit(f.edge.authenticateNetwork(r), app.ID)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				if _, err := f.edge.apply(context.Background(), app.Owner, Request{Action: "private", ID: app.ID}); err != nil {
					t.Fatal(err)
				}
				if revoked := admitted.Context().Err() != nil; revoked != (site == "same-site") {
					t.Fatalf("request revoked=%v for %s", revoked, site)
				}
			})
		}
	}
}
