package daemon

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	meshserve "github.com/shaul/mesh/internal/serve"
)

func TestPrivateHTTPRejectsRebindingHost(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "private-file.txt"), []byte("private fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := meshserve.NewRegistry([]meshserve.Service{{Name: "files", Kind: meshserve.Files, Target: root}})
	if err != nil {
		t.Fatal(err)
	}
	for _, surface := range []string{"tailnet HTTP", "loopback HTTPS"} {
		t.Run(surface, func(t *testing.T) {
			var handler http.Handler
			if surface == "tailnet HTTP" {
				handler = newWebSocketServer(context.Background(), listenerConfig{
					webSocketPath: "/mesh", httpHandler: appOriginHandler(nil, registry),
					tailnetAddrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}, tailnetPort: 7337,
				}, newConnectionGroup(echoOneFrame)).Handler
			} else {
				handler = serviceOnlyHTTPSHandler(listenerConfig{
					webSocketPath: "/mesh", httpHandler: appOriginHandler(nil, registry), httpsPort: 7337,
				})
			}
			server := httptest.NewUnstartedServer(handler)
			if surface == "loopback HTTPS" {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/files/", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Host = "rebind.attacker.example:7337"
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close() //nolint:errcheck // test response cleanup
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			listing := strings.Contains(string(body), "private-file.txt")
			t.Logf("Host %q returned %d; private Files listing exposed = %t", request.Host, response.StatusCode, listing)
			if response.StatusCode != http.StatusMisdirectedRequest || listing {
				t.Fatalf("rebinding response = %d, private listing = %t; want 421 and no listing", response.StatusCode, listing)
			}
		})
	}
}

func TestPrivateHTTPHostAllowlist(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		edge    bool
		allowed bool
	}{
		{name: "bound IPv4", host: "100.64.0.1", allowed: true},
		{name: "bound IPv4 with listener port", host: "100.64.0.1:7337", allowed: true},
		{name: "bound IPv4 forwarded port", host: "100.64.0.1:7338", allowed: true},
		{name: "mapped IPv4", host: "[::ffff:100.64.0.1]:7337", allowed: true},
		{name: "bound IPv6", host: "[fd7a:115c:a1e0::1]", allowed: true},
		{name: "bound IPv6 with listener port", host: "[fd7a:115c:a1e0::1]:7337", allowed: true},
		{name: "other IPv4", host: "100.64.0.2:7337"},
		{name: "other IPv6", host: "[fd7a:115c:a1e0::2]:7337"},
		{name: "loopback", host: "127.0.0.1:7337"},
		{name: "MagicDNS full name", host: "pc.example.ts.net", allowed: true},
		{name: "MagicDNS short name", host: "pc", allowed: true},
		{name: "MagicDNS case and dot", host: "PC.EXAMPLE.TS.NET.:7337", allowed: true},
		{name: "other MagicDNS host", host: "other.example.ts.net"},
		{name: "private name", host: "pc.mesh.shaulavo.dev", allowed: true},
		{name: "private external TLS port", host: "pc.mesh.shaulavo.dev:443", allowed: true},
		{name: "private route subdomain", host: "blog.pc.mesh.shaulavo.dev", allowed: true},
		{name: "nested private route subdomain", host: "nested.blog.pc.mesh.shaulavo.dev"},
		{name: "private name suffix attack", host: "evilpc.mesh.shaulavo.dev"},
		{name: "other private host", host: "other.mesh.shaulavo.dev"},
		{name: "private base wildcard", host: "mesh.shaulavo.dev"},
		{name: "public pinned edge", host: "blog.shaulavo.dev", edge: true, allowed: true},
		{name: "public pinned edge external port", host: "blog.shaulavo.dev:443", edge: true, allowed: true},
		{name: "public untrusted peer", host: "blog.shaulavo.dev"},
		{name: "public apex", host: "shaulavo.dev", edge: true},
		{name: "nested public name", host: "nested.blog.shaulavo.dev", edge: true},
		{name: "attacker", host: "rebind.attacker.example:7337"},
		{name: "attacker from edge", host: "rebind.attacker.example:7337", edge: true},
		{name: "malformed port", host: "pc.mesh.shaulavo.dev:bad"},
		{name: "empty port", host: "pc.mesh.shaulavo.dev:"},
		{name: "oversized port", host: "pc.mesh.shaulavo.dev:65536"},
		{name: "invalid DNS label", host: "bad_.pc.mesh.shaulavo.dev"},
		{name: "empty DNS label", host: "pc..mesh.shaulavo.dev"},
		{name: "userinfo", host: "attacker@pc.mesh.shaulavo.dev"},
	}
	for _, surface := range []string{"tailnet HTTP", "loopback HTTPS"} {
		t.Run(surface, func(t *testing.T) {
			for _, test := range cases {
				t.Run(test.name, func(t *testing.T) {
					var dispatches atomic.Int32
					cfg := listenerConfig{
						webSocketPath: "/mesh", tailnetPort: 7337, httpsPort: 7337,
						httpHandler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							dispatches.Add(1)
							w.WriteHeader(http.StatusNoContent)
						}),
						httpHosts: httpHostPolicy{
							tailnetAddrs: []netip.Addr{netip.MustParseAddr("100.64.0.1"), netip.MustParseAddr("fd7a:115c:a1e0::1")},
							tailnetNames: []string{"pc.example.ts.net", "pc"},
							privateName:  func() string { return "pc.mesh.shaulavo.dev" },
							trustPublicEdgeForwarding: func(address netip.Addr) bool {
								return test.edge && address == netip.MustParseAddr("127.0.0.1")
							},
						},
					}
					handler := newWebSocketServer(context.Background(), cfg, newConnectionGroup(echoOneFrame)).Handler
					if surface == "loopback HTTPS" {
						handler = serviceOnlyHTTPSHandler(cfg)
					}
					server := httptest.NewUnstartedServer(handler)
					if surface == "loopback HTTPS" {
						server.StartTLS()
					} else {
						server.Start()
					}
					defer server.Close()
					request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/service", nil)
					if err != nil {
						t.Fatal(err)
					}
					request.Host = test.host
					request.Header.Set("X-Forwarded-Host", "pc.mesh.shaulavo.dev")
					request.Header.Set("X-Forwarded-For", "100.64.0.9")
					response, err := server.Client().Do(request)
					if err != nil {
						t.Fatal(err)
					}
					_ = response.Body.Close()
					wantStatus, wantDispatches := http.StatusMisdirectedRequest, int32(0)
					if test.allowed {
						wantStatus, wantDispatches = http.StatusNoContent, 1
					}
					if response.StatusCode != wantStatus || dispatches.Load() != wantDispatches {
						t.Fatalf("Host %q: status = %d, dispatches = %d; want %d, %d", test.host, response.StatusCode, dispatches.Load(), wantStatus, wantDispatches)
					}
				})
			}
		})
	}
}

func TestPrivateHTTPHostPolicyUsesCurrentIdentity(t *testing.T) {
	var privateName atomic.Pointer[string]
	var edgeAddress atomic.Pointer[netip.Addr]
	policy := httpHostPolicy{
		privateName: func() string {
			if name := privateName.Load(); name != nil {
				return *name
			}
			return ""
		},
		trustPublicEdgeForwarding: func(address netip.Addr) bool {
			pinned := edgeAddress.Load()
			return pinned != nil && *pinned == address
		},
	}
	request := httptest.NewRequest(http.MethodGet, "http://pc.mesh.shaulavo.dev/files/", nil)
	if policy.accepts(request) {
		t.Fatal("accepted private name before ingress was ready")
	}
	name := "pc.mesh.shaulavo.dev"
	privateName.Store(&name)
	if !policy.accepts(request) {
		t.Fatal("installed private name was not accepted without a restart")
	}
	privateName.Store(nil)
	if policy.accepts(request) {
		t.Fatal("accepted private name after it was withdrawn")
	}
	request.Host, request.RemoteAddr = "blog.shaulavo.dev", "100.64.0.9:40000"
	if policy.accepts(request) {
		t.Fatal("accepted public name before the edge was pinned")
	}
	edge := netip.MustParseAddr("100.64.0.9")
	edgeAddress.Store(&edge)
	if !policy.accepts(request) {
		t.Fatal("pinned edge was not accepted without a restart")
	}
	request.RemoteAddr = "[::ffff:100.64.0.9]:40000"
	if !policy.accepts(request) {
		t.Fatal("IPv4-mapped immediate edge address was not accepted")
	}
	request.RemoteAddr = "not-an-address"
	if policy.accepts(request) {
		t.Fatal("accepted public name with an invalid immediate peer")
	}
	edgeAddress.Store(nil)
	request.RemoteAddr = "100.64.0.9:40000"
	if policy.accepts(request) {
		t.Fatal("accepted public name after the edge pin was withdrawn")
	}
}

type hostPolicyDemandGate struct {
	entered atomic.Int32
}

func (g *hostPolicyDemandGate) Enter(context.Context, string) (func(), error) {
	g.entered.Add(1)
	return func() {}, nil
}

func TestPrivateHTTPRejectsBeforeProxyAndDemand(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := meshserve.NewRegistry([]meshserve.Service{
		{Name: "proxy", Kind: meshserve.Proxy, Target: port},
		{Name: "demand", Kind: meshserve.Proxy, Target: port, Demand: &meshserve.Demand{Command: "fixture", Cwd: t.TempDir()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gate := &hostPolicyDemandGate{}
	registry.SetDemandGate(gate)
	cfg := listenerConfig{
		webSocketPath: "/mesh", httpHandler: appOriginHandler(nil, registry), tailnetPort: 7337, httpsPort: 7337,
		httpHosts: httpHostPolicy{tailnetAddrs: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
	}
	for _, handler := range []http.Handler{
		newWebSocketServer(context.Background(), cfg, newConnectionGroup(echoOneFrame)).Handler,
		serviceOnlyHTTPSHandler(cfg),
	} {
		for _, route := range []string{"proxy", "demand"} {
			request := httptest.NewRequest(http.MethodGet, "http://rebind.attacker.example:7337/"+route+"/", nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusMisdirectedRequest || hits.Load() != 0 || gate.entered.Load() != 0 {
				t.Fatalf("rejected /%s status = %d, upstream hits = %d, demand starts = %d", route, response.Code, hits.Load(), gate.entered.Load())
			}
		}
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7337/demand/", nil)
	response := httptest.NewRecorder()
	newWebSocketServer(context.Background(), cfg, newConnectionGroup(echoOneFrame)).Handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || hits.Load() != 1 || gate.entered.Load() != 1 {
		t.Fatalf("allowed demand status = %d, upstream hits = %d, demand starts = %d", response.Code, hits.Load(), gate.entered.Load())
	}
}

func TestPrivateRequestHostRejectsMalformedAuthorities(t *testing.T) {
	for _, authority := range []string{
		"", "[fd7a:115c:a1e0::1%lo]", "[fd7a:115c:a1e0::1%lo]:7337", "fd7a:115c:a1e0::1%lo",
		"[100.64.0.1]", "[pc.mesh.shaulavo.dev]", "pc.mesh.shaulavo.dev:0", "pc.mesh.shaulavo.dev:+7337",
		"pc.mesh.shaulavo.dev ", " pc.mesh.shaulavo.dev", "pc.mesh.shaulavo.dev..", "-bad.pc.mesh.shaulavo.dev",
		strings.Repeat("a", 64) + ".pc.mesh.shaulavo.dev", "pc.mesh.shaulavo.dev/path", "pc.mesh.shaulavo.dev#fragment",
	} {
		t.Run(authority, func(t *testing.T) {
			if host, ok := privateRequestHost(authority); ok {
				t.Fatalf("malformed authority %q accepted as %q", authority, host)
			}
		})
	}
}

func TestServePrivateHTTPUsesBoundAuthorities(t *testing.T) {
	port := reserveTCPPort(t, "127.0.0.1")
	blocked, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.2", strconv.Itoa(int(port))))
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close() //nolint:errcheck // test listener cleanup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runRuntime(t, ctx, ListenerConfig{
		StateDir: t.TempDir(), TailnetPort: port, TailnetAddrs: []string{"127.0.0.1", "127.0.0.2"}, WebSocketPath: "/mesh",
		TailnetNames: []string{"pc.example.ts.net", "pc"},
		PrivateName:  func() string { return "pc.mesh.shaulavo.dev" },
		TrustPublicEdgeForwarding: func(address netip.Addr) bool {
			return address == netip.MustParseAddr("127.0.0.1")
		},
		ReportError: func(err error) { t.Log(err) },
		HTTPHandler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	}, echoOneFrame)
	waitForTCPRuntime(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
	for _, test := range []struct {
		host string
		want int
	}{
		{host: "127.0.0.1", want: http.StatusNoContent},
		{host: net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), want: http.StatusNoContent},
		{host: "127.0.0.2", want: http.StatusMisdirectedRequest},
		{host: "pc.example.ts.net", want: http.StatusNoContent},
		{host: "pc", want: http.StatusNoContent},
		{host: "pc.mesh.shaulavo.dev:443", want: http.StatusNoContent},
		{host: "blog.shaulavo.dev:443", want: http.StatusNoContent},
		{host: "rebind.attacker.example:7337", want: http.StatusMisdirectedRequest},
	} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(int(port))+"/service", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = test.host
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != test.want {
			t.Fatalf("Host %q returned %d, want %d", test.host, response.StatusCode, test.want)
		}
	}
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateHTTPDoesNotReplaceWebSocketOriginRefusal(t *testing.T) {
	cfg := listenerConfig{webSocketPath: "/mesh", tailnetPort: 7337, httpsPort: 443}
	request := httptest.NewRequest(http.MethodGet, "http://rebind.attacker.example:7337/mesh", nil)
	request.Header.Set("Origin", "http://rebind.attacker.example:7337")
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	response := httptest.NewRecorder()
	newWebSocketServer(context.Background(), cfg, newConnectionGroup(echoOneFrame)).Handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("WebSocket Origin refusal = %d, want 403", response.Code)
	}
	response = httptest.NewRecorder()
	serviceOnlyHTTPSHandler(cfg).ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("HTTPS WebSocket refusal = %d, want 404", response.Code)
	}
}
