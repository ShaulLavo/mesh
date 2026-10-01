package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestControlConnectionCapsRefuseWithoutDisruptingClients(t *testing.T) {
	for _, network := range []string{"Unix", "Tailnet"} {
		t.Run(network, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), runtimeTestTimeout)
			defer cancel()
			cfg := ListenerConfig{StateDir: t.TempDir(), UnixConnectionLimit: 1, TailnetConnectionLimit: 1}
			dial := func() transport.Conn { return dialUnixRuntime(t, filepath.Join(cfg.StateDir, daemonSocketName)) }
			var listeners []net.Listener
			if network == "Tailnet" {
				listener, port := newTCPListener(t, "127.0.0.1:0")
				other, _ := newTCPListener(t, fmt.Sprintf("127.0.0.2:%d", port))
				listeners = append(listeners, listener, other)
				cfg.TailnetAddrs = []string{"127.0.0.1", "127.0.0.2"}
				cfg.TailnetPort = port
				cfg.WebSocketPath = "/mesh"
				endpoint := "ws://" + listener.Addr().String() + "/mesh"
				dial = func() transport.Conn {
					conn, err := transport.DialOnce(ctx, endpoint, transport.DialOptions{})
					if err != nil {
						t.Fatal(err)
					}
					return conn
				}
			}
			done := runRuntime(t, ctx, cfg, echoControlFrames, listeners...)
			first := dial()
			defer first.Close() //nolint:errcheck // test cleanup
			assertControlEcho(t, first)
			if network == "Unix" {
				assertUnixCapRefusal(t, dial())
			} else {
				assertTailnetCapRefusal(ctx, t, listeners[0].Addr().String())
				assertTailnetCapRefusal(ctx, t, listeners[1].Addr().String())
				local := dialUnixRuntime(t, filepath.Join(cfg.StateDir, daemonSocketName))
				defer local.Close() //nolint:errcheck // test cleanup
				assertControlEcho(t, local)
			}
			assertControlEcho(t, first)
			cancel()
			if err := waitRuntime(t, done); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTailnetRefusalNamesConfiguredCap(t *testing.T) {
	group := newConnectionGroup(echoControlFrames)
	group.limit, group.name = 1, "Tailnet"
	id, err := group.reserve()
	if err != nil {
		t.Fatal(err)
	}
	defer group.release(id)
	server := newWebSocketServer(context.Background(), listenerConfig{webSocketPath: "/mesh"}, group)
	request := httptest.NewRequest(http.MethodGet, "/mesh", nil)
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "Tailnet control connection cap (1)") {
		t.Fatalf("refusal = %d %s", response.Code, response.Body.String())
	}
}

func echoControlFrames(_ context.Context, conn transport.Conn) error {
	for {
		frame, err := conn.ReadFrame()
		if err != nil {
			return fmt.Errorf("echo read: %w", err)
		}
		if err := conn.WriteFrame(frame); err != nil {
			return fmt.Errorf("echo write: %w", err)
		}
	}
}

func assertControlEcho(t *testing.T, conn transport.Conn) {
	t.Helper()
	payload, err := (protocol.Control{Type: protocol.TypeHostInfo, RequestID: "cap-test"}).Encode()
	frame := protocol.Frame{Kind: protocol.KindControl, Payload: payload}
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteFrame(frame); err != nil {
		t.Fatal(err)
	}
	got, err := conn.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	assertRuntimeFrame(t, got, frame)
}

func TestConnectionReservationsBoundRacesAndRecoverCapacity(t *testing.T) {
	group := newConnectionGroup(echoControlFrames)
	group.name, group.limit = "Tailnet", 8
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			if _, err := group.reserve(); err == nil {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 8 {
		t.Fatalf("admitted = %d, want 8", admitted.Load())
	}
	for id := range group.conns {
		group.release(id)
	}
	id, err := group.reserve()
	if err != nil {
		t.Fatalf("capacity after release: %v", err)
	}
	if err := group.closeAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := group.reserve(); err == nil {
		t.Fatal("shutdown allowed new reservation")
	}
	group.release(id)
	group.wait()
}

func TestInvalidWebSocketUpgradeReturnsReservation(t *testing.T) {
	group := newConnectionGroup(echoControlFrames)
	group.name, group.limit = "Tailnet", 1
	server := newWebSocketServer(context.Background(), listenerConfig{webSocketPath: "/mesh"}, group)
	server.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/mesh", nil))
	id, err := group.reserve()
	if err != nil {
		t.Fatalf("invalid upgrade held capacity: %v", err)
	}
	group.release(id)
}

func TestControlConnectionLimitDefaultsAndValidation(t *testing.T) {
	cfg := ListenerConfig{StateDir: t.TempDir()}
	normalized, err := validateListenerConfig(context.Background(), cfg, echoControlFrames)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.unixConnectionLimit != DefaultUnixConnectionLimit || normalized.tailnetConnectionLimit != DefaultTailnetConnectionLimit {
		t.Fatalf("unexpected caps: Unix %d, Tailnet %d", normalized.unixConnectionLimit, normalized.tailnetConnectionLimit)
	}
	for _, caps := range [][2]int{{-1, 1}, {1, -1}} {
		cfg.UnixConnectionLimit, cfg.TailnetConnectionLimit = caps[0], caps[1]
		if _, err := validateListenerConfig(context.Background(), cfg, echoControlFrames); err == nil {
			t.Fatal("negative cap accepted")
		}
	}
}

func assertUnixCapRefusal(t *testing.T, refused transport.Conn) {
	t.Helper()
	defer refused.Close() //nolint:errcheck // test cleanup
	payload, err := (protocol.Control{Type: protocol.TypeHostInfo, RequestID: "refused"}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := refused.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	frame, err := refused.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	message, err := protocol.DecodeControl(frame.Payload)
	if err != nil || message.Type != protocol.TypeError || message.RequestID != "refused" || !strings.Contains(message.Message, "Unix control connection cap (1)") {
		t.Fatalf("refusal = %+v, %v", message, err)
	}
}

func assertTailnetCapRefusal(ctx context.Context, t *testing.T, address string) {
	t.Helper()
	endpoint := "ws://" + address + "/mesh"
	conn, err := transport.DialOnce(ctx, endpoint, transport.DialOptions{})
	if err == nil {
		_ = conn.Close()
		t.Fatal("excess WebSocket connection accepted")
	}
	if !strings.Contains(err.Error(), "Tailnet control connection cap (1)") {
		t.Fatalf("dial refusal = %v", err)
	}
	response, err := http.Get("http://" + address + "/mesh") //nolint:gosec // loopback fixture
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck // test cleanup
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.StatusCode)
	}
}
