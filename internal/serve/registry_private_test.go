package serve

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRegistryPrivateCrossSitePOST(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	_, port, err := net.SplitHostPort(upstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry([]Service{{Name: "api", Kind: Proxy, Target: port}})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewTLSServer(registry)
	defer front.Close()
	request, err := http.NewRequest(http.MethodPost, front.URL+"/api/mutate", strings.NewReader("confirm=yes"))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "pc.mesh.mesh.test"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	request.Header.Set("Sec-Fetch-Mode", "no-cors")
	response, err := front.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusForbidden || hits.Load() != 0 {
		t.Fatalf("private cross-site POST status=%d, upstream hits=%d; want 403 and zero", response.StatusCode, hits.Load())
	}
}

func TestRegistryBrowserRequests(t *testing.T) {
	for _, scope := range []struct {
		name, host  string
		tls, public bool
	}{
		{name: "tailnet HTTP", host: "100.64.0.1:7337"},
		{name: "private HTTPS", host: "pc.mesh.mesh.test", tls: true},
		{name: "public HTTPS", host: "app.mesh.test", tls: true, public: true},
	} {
		t.Run(scope.name, func(t *testing.T) {
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				w.Header().Set("X-Seen-Body", string(body))
				w.Header().Set("X-Seen-Method", r.Method)
				w.Header().Set("X-Seen-Path", r.URL.Path)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			_, port, err := net.SplitHostPort(upstream.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			registry, err := NewRegistry([]Service{{Name: "api", Kind: Proxy, Target: port, PublicName: "app.mesh.test"}})
			if err != nil {
				t.Fatal(err)
			}
			front := httptest.NewUnstartedServer(registry)
			if scope.tls {
				front.StartTLS()
			} else {
				front.Start()
			}
			defer front.Close()
			ownOrigin := "http://" + scope.host
			wrongScheme := "https://" + scope.host
			if scope.tls {
				ownOrigin, wrongScheme = wrongScheme, ownOrigin
			}
			for _, tc := range []struct {
				name, method, site, mode, dest, origin string
				allow                                  bool
			}{
				{name: "cross-site POST", method: "POST", site: "cross-site", origin: "https://attacker.example"},
				{name: "same-site sibling POST", method: "POST", site: "same-site", origin: "https://sibling.mesh.test"},
				{name: "cross-site POST without Origin", method: "POST", site: "cross-site"},
				{name: "same-origin fetch", method: "POST", site: "same-origin", origin: ownOrigin, allow: true},
				{name: "same-origin GET without Origin", method: "GET", site: "same-origin", allow: true},
				{name: "cross-site top-level GET", method: "GET", site: "cross-site", mode: "navigate", dest: "document", allow: true},
				{name: "same-site top-level HEAD", method: "HEAD", site: "same-site", mode: "navigate", dest: "document", allow: true},
				{name: "direct navigation", method: "GET", site: "none", allow: true},
				{name: "cross-site form POST", method: "POST", site: "cross-site", mode: "navigate", dest: "document"},
				{name: "cross-site iframe", method: "GET", site: "cross-site", mode: "navigate", dest: "iframe"},
				{name: "cross-site image", method: "GET", site: "cross-site", mode: "no-cors", dest: "image"},
				{name: "same-site fetch", method: "GET", site: "same-site"},
				{name: "unknown site fetch", method: "GET", site: "invalid"},
				{name: "curl POST", method: "POST", allow: true},
				{name: "legacy same-origin PATCH", method: "PATCH", origin: ownOrigin, allow: true},
				{name: "legacy foreign POST", method: "POST", origin: "https://attacker.example"},
				{name: "legacy foreign GET", method: "GET", origin: "https://attacker.example"},
				{name: "foreign OPTIONS", method: "OPTIONS", origin: "https://attacker.example"},
				{name: "null Origin", method: "DELETE", origin: "null"},
				{name: "same-origin no-referrer form", method: "POST", site: "same-origin", mode: "navigate", dest: "document", origin: "null", allow: true},
				{name: "cross-site null Origin", method: "POST", site: "cross-site", mode: "navigate", dest: "document", origin: "null"},
				{name: "same-site null Origin", method: "POST", site: "same-site", mode: "navigate", dest: "document", origin: "null"},
				{name: "null Origin without fetch metadata", method: "POST", origin: "null"},
				{name: "conflicting same-origin metadata", method: "POST", site: "same-origin", origin: "https://attacker.example"},
				{name: "conflicting navigation metadata", method: "GET", site: "cross-site", mode: "navigate", dest: "document", origin: "https://attacker.example"},
				{name: "wrong scheme despite forwarded headers", method: "POST", origin: wrongScheme},
			} {
				t.Run(tc.name, func(t *testing.T) {
					before := hits.Load()
					request, err := http.NewRequest(tc.method, front.URL+"/api/mutate", strings.NewReader("confirm=yes"))
					if err != nil {
						t.Fatal(err)
					}
					request.Host = scope.host
					request.Header.Set("Sec-Fetch-Site", tc.site)
					request.Header.Set("Sec-Fetch-Mode", tc.mode)
					request.Header.Set("Sec-Fetch-Dest", tc.dest)
					request.Header.Set("Origin", tc.origin)
					request.Header.Set("X-Forwarded-Proto", strings.SplitN(wrongScheme, ":", 2)[0])
					request.Header.Set("X-Forwarded-Host", "attacker.example")
					response, err := front.Client().Do(request)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = response.Body.Close() }()
					_, _ = io.Copy(io.Discard, response.Body)
					wantStatus, wantHits := http.StatusForbidden, int32(0)
					if scope.public || tc.allow {
						wantStatus, wantHits = http.StatusNoContent, 1
					}
					if response.StatusCode != wantStatus || hits.Load()-before != wantHits {
						t.Fatalf("status=%d, upstream hits=%d; want status=%d, upstream hits=%d", response.StatusCode, hits.Load()-before, wantStatus, wantHits)
					}
					if wantHits != 0 && (response.Header.Get("X-Seen-Method") != tc.method || response.Header.Get("X-Seen-Body") != "confirm=yes" || response.Header.Get("X-Seen-Path") != "/mutate") {
						t.Fatalf("upstream method, body, or path changed: %v", response.Header)
					}
				})
			}
		})
	}
}

func TestRegistryPrivateGatePrecedesRouteHandlers(t *testing.T) {
	for _, kind := range []Kind{Static, Files, Proxy} {
		for _, path := range []string{"/service", "/service/"} {
			t.Run(string(kind)+path, func(t *testing.T) {
				service := Service{Name: "service", Kind: kind, Target: t.TempDir()}
				if kind == Proxy {
					service.Target = "12345"
					service.Demand = &Demand{Command: "test-command", Cwd: t.TempDir()}
				}
				registry, err := NewRegistry([]Service{service})
				if err != nil {
					t.Fatal(err)
				}
				gate := &fakeGate{err: errString("unexpected on-demand start")}
				registry.SetDemandGate(gate, nil)
				request := httptest.NewRequest(http.MethodPost, "https://pc.mesh.mesh.test"+path, nil)
				request.Header.Set("Origin", "https://attacker.example")
				request.Header.Set("Sec-Fetch-Site", "cross-site")
				response := httptest.NewRecorder()
				registry.ServeHTTP(response, request)
				if response.Code != http.StatusForbidden || len(gate.entered) != 0 || response.Header().Get("Location") != "" {
					t.Fatalf("status=%d, demand starts=%v, redirect=%q; want 403, no starts or redirect", response.Code, gate.entered, response.Header().Get("Location"))
				}
			})
		}
	}
}

func TestRegistryWebSocketOrigins(t *testing.T) {
	for _, public := range []bool{false, true} {
		host := "pc.mesh.mesh.test"
		if public {
			host = "app.mesh.test"
		}
		t.Run(host, func(t *testing.T) {
			var hits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
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
				if err := conn.Write(r.Context(), kind, message); err != nil {
					t.Error(err)
				}
			}))
			defer upstream.Close()
			_, port, err := net.SplitHostPort(upstream.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			registry, err := NewRegistry([]Service{{Name: "api", Kind: Proxy, Target: port, PublicName: "app.mesh.test"}})
			if err != nil {
				t.Fatal(err)
			}
			front := httptest.NewTLSServer(registry)
			defer front.Close()
			for _, origin := range []string{"https://" + host, "https://sibling.mesh.test", "https://attacker.example", "null", ""} {
				name := origin
				if name == "" {
					name = "headerless client"
				}
				t.Run(name, func(t *testing.T) {
					before := hits.Load()
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					headers := make(http.Header)
					if origin != "" {
						headers.Set("Origin", origin)
						headers.Set("Sec-Fetch-Site", map[string]string{
							"https://" + host:           "same-origin",
							"https://sibling.mesh.test": "same-site",
							"https://attacker.example":  "cross-site",
							"null":                      "same-origin",
						}[origin])
					}
					conn, response, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(front.URL, "https")+"/api/socket", &websocket.DialOptions{
						HTTPClient: front.Client(), Host: host, HTTPHeader: headers,
					})
					if response != nil && response.Body != nil {
						defer func() { _ = response.Body.Close() }()
					}
					if !public && origin != "" && origin != "https://"+host {
						if err == nil {
							_ = conn.CloseNow()
							t.Fatal("cross-origin WebSocket was accepted")
						}
						if response == nil || response.StatusCode != http.StatusForbidden || hits.Load() != before {
							t.Fatalf("WebSocket not refused before upstream: err=%v, response=%v, hits=%d", err, response, hits.Load()-before)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = conn.CloseNow() }()
					if err := conn.Write(ctx, websocket.MessageText, []byte("owner message")); err != nil {
						t.Fatal(err)
					}
					_, message, err := conn.Read(ctx)
					if err != nil || string(message) != "owner message" || hits.Load()-before != 1 {
						t.Fatalf("WebSocket echo=%q, err=%v, hits=%d; want owner message and one", message, err, hits.Load()-before)
					}
				})
			}
		})
	}
}
