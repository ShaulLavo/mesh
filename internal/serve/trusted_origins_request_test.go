package serve

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestPrivateTrustedOriginAdmitsCORSAndPreservesOrigin(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if got := r.Header.Get("Origin"); got != "https://bun-demo.sprockt.dev" {
			t.Errorf("upstream Origin = %q", got)
		}
		w.Header().Set("X-Upstream-Method", r.Method)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	_, port, err := net.SplitHostPort(upstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry([]Service{{Name: "api", Kind: Proxy, Target: port, PrivateHost: "api.mesh.test", TrustedOrigins: []string{"https://bun-demo.sprockt.dev"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodOptions, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			request := httptest.NewRequest(method, "https://api.mesh.test/mutate", strings.NewReader("ok"))
			request.Host = "api.mesh.test"
			request.Header.Set("Origin", "https://bun-demo.sprockt.dev")
			request.Header.Set("Sec-Fetch-Site", "cross-site")
			response := httptest.NewRecorder()
			registry.ServeHTTP(response, request)
			if response.Code != http.StatusNoContent || response.Header().Get("X-Upstream-Method") != method || hits.Load() == 0 {
				t.Fatalf("status=%d method=%q hits=%d", response.Code, response.Header().Get("X-Upstream-Method"), hits.Load())
			}
		})
	}
}

func TestPrivateTrustedOriginRejectsExactMismatchesAndMultipleHeaders(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer upstream.Close()
	_, port, _ := net.SplitHostPort(upstream.Listener.Addr().String())
	registry, err := NewRegistry([]Service{{Name: "api", Kind: Proxy, Target: port, PrivateHost: "api.mesh.test", TrustedOrigins: []string{"https://bun-demo.sprockt.dev"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		origins   []string
		fetchSite string
		wantCode  int
	}{
		{name: "wrong origin", origins: []string{"https://bun-demo.sprockt.dev/"}, fetchSite: "cross-site", wantCode: http.StatusForbidden},
		{name: "cross-site originless", fetchSite: "cross-site", wantCode: http.StatusForbidden},
		{name: "multiple origins", origins: []string{"https://bun-demo.sprockt.dev", "https://other.example"}, wantCode: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := hits.Load()
			request := httptest.NewRequest(http.MethodPost, "https://api.mesh.test/mutate", nil)
			for _, origin := range test.origins {
				request.Header.Add("Origin", origin)
			}
			request.Header.Set("Sec-Fetch-Site", test.fetchSite)
			response := httptest.NewRecorder()
			registry.ServeHTTP(response, request)
			if response.Code != test.wantCode || hits.Load() != before {
				t.Fatalf("status=%d hits=%d; want status=%d and no upstream", response.Code, hits.Load()-before, test.wantCode)
			}
		})
	}
}

func TestPrivateTrustedOriginWebSocket(t *testing.T) {
	var seenOrigin atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenOrigin.Store(r.Header.Get("Origin"))
		connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = connection.CloseNow() }()
		kind, message, err := connection.Read(r.Context())
		if err != nil {
			return
		}
		_ = connection.Write(r.Context(), kind, message)
	}))
	defer upstream.Close()
	_, port, _ := net.SplitHostPort(upstream.Listener.Addr().String())
	registry, err := NewRegistry([]Service{{Name: "api", Kind: Proxy, Target: port, PrivateHost: "api.mesh.test", TrustedOrigins: []string{"https://bun-demo.sprockt.dev"}}})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewTLSServer(registry)
	defer front.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	headers := http.Header{"Origin": []string{"https://bun-demo.sprockt.dev"}, "Sec-Fetch-Site": []string{"cross-site"}}
	connection, response, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(front.URL, "https")+"/socket", &websocket.DialOptions{HTTPClient: front.Client(), Host: "api.mesh.test", HTTPHeader: headers})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatal(err)
	}
	defer func() { _ = connection.CloseNow() }()
	if err := connection.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	_, message, err := connection.Read(ctx)
	if err != nil || string(message) != "hello" {
		t.Fatalf("echo=%q err=%v", message, err)
	}
	if got := seenOrigin.Load(); got != "https://bun-demo.sprockt.dev" {
		t.Fatalf("upstream Origin = %v", got)
	}
}

func TestPrivateTrustedOriginDoesNotBypassSNI(t *testing.T) {
	registry, err := NewRegistry([]Service{{Name: "api", Kind: Proxy, Target: "1", PrivateHost: "api.mesh.test", TrustedOrigins: []string{"https://bun-demo.sprockt.dev"}}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodOptions, "https://api.mesh.test/", nil)
	request.TLS = &tls.ConnectionState{ServerName: "other.mesh.test"}
	request.Header.Set("Origin", "https://bun-demo.sprockt.dev")
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, request)
	if response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("trusted origin SNI mismatch status=%d, want 421", response.Code)
	}
}
