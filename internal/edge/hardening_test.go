package edge

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUnknownLoopbackSenderCannotRotateQuotaWithXFF(t *testing.T) {
	registry := testRegistry(t, ModeProxy, time.Now())
	defer registry.Close()
	for i := 0; i <= maximumRequestsPerMinute; i++ {
		request := publicRequest(http.MethodGet, "app.shaulavo.dev", "/")
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i+1))
		request.Header.Set("X-Forwarded-Proto", "https")
		response := httptest.NewRecorder()
		registry.ServeHTTP(response, request)
		want := http.StatusNotFound
		if i == maximumRequestsPerMinute {
			want = http.StatusTooManyRequests
		}
		if response.Code != want {
			t.Fatalf("request %d got %d, want %d", i, response.Code, want)
		}
	}
}

func TestPublicProxiesDropSharedParentCookies(t *testing.T) {
	for _, kind := range []string{"service", "tunnel"} {
		t.Run(kind, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for _, cookie := range []string{
					"evil=1; Domain=shaulavo.dev; Path=/",
					"evil=2; dOmAiN = .SHAULAVO.DEV; Path=/",
					"evil=3; Domain=\"shaulavo.dev\"; Path=/",
					"evil=4; Domain=shaulavo.dev; Domain=docs.shaulavo.dev; Path=/",
					"host=ok; Path=/; Secure; HttpOnly; SameSite=Lax",
					"scoped=ok; Domain=docs.shaulavo.dev; Secure; HttpOnly; SameSite=Lax",
				} {
					w.Header().Add("Set-Cookie", cookie)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer backend.Close()
			name := "docs.shaulavo.dev"
			requestPath := "/docs/"
			registry := testRegistry(t, ModeDirectTLS, time.Now())
			defer registry.Close()
			if kind == "tunnel" {
				controller, tunnelRegistry, claimant := newProxyTunnel(t)
				activateProxyTunnel(t, controller, claimant, proxyEndpointFor(backend))
				registry = tunnelRegistry
				name = proxyTunnelName
				requestPath = "/"
			} else {
				now := time.Now()
				origin, _ := testIdentity(t)
				if err := registry.Replace([]PublishedRoute{{Route: Route{PublicName: name, ServiceName: "docs"}, Origin: testResolvedOrigin(origin, testHTTPServerEndpoint(t, backend), now)}}); err != nil {
					t.Fatal(err)
				}
			}
			capture := &eventCapture{}
			registry.logger.Close()
			registry.logger = newEventLogger(log.New(capture, "", 0), time.Now)
			response := httptest.NewRecorder()
			registry.ServeHTTP(response, publicRequest(http.MethodGet, name, requestPath))
			if response.Code != http.StatusNoContent {
				t.Fatalf("status %d", response.Code)
			}
			cookies := response.Header().Values("Set-Cookie")
			if len(cookies) != 2 || !strings.HasPrefix(cookies[0], "host=ok") || !strings.HasPrefix(cookies[1], "scoped=ok") {
				t.Fatalf("unsafe or lost cookies: %q", cookies)
			}
			deadline := time.Now().Add(time.Second)
			for !capture.contains("event=cookies-stripped count=4") && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if !capture.contains("event=cookies-stripped count=4") {
				t.Fatal("stripped cookie count was not logged")
			}
		})
	}
}

type eventCapture struct {
	mu    sync.Mutex
	lines []string
}

func (w *eventCapture) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, string(p))
	return len(p), nil
}
func (w *eventCapture) contains(s string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range w.lines {
		if strings.Contains(line, s) {
			return true
		}
	}
	return false
}

func TestEventFloodDoesNotHideOtherCategoryAndReportsDrops(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	writer := &eventCapture{}
	logger := newEventLogger(log.New(writer, "", 0), func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	defer logger.Close()
	for i := 0; i < 1000; i++ {
		logger.Print("edge event=invalid-public-host")
	}
	logger.Print("edge event=origin-unavailable origin=test")
	mu.Lock()
	now = now.Add(eventWindow)
	mu.Unlock()
	logger.Print("edge event=invalid-public-host")
	deadline := time.Now().Add(time.Second)
	for (!writer.contains("event=origin-unavailable") || !writer.contains("event=events-dropped category=invalid-public-host dropped=")) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !writer.contains("event=origin-unavailable") {
		t.Error("host flood hid origin failure")
	}
	if !writer.contains("event=events-dropped category=invalid-public-host dropped=") {
		t.Error("dropped events were silent")
	}
}
