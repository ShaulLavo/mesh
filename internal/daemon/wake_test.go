package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/wake"
)

func TestWakeCrossesWebSocketAndHonorsTargetRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	power, authority, sends := testWakeController(t)
	grant, err := authority.SetAllowed(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := mustServerTestLifecycle(t, &serverTestCatalog{}, failingServerTestConnector())
	server, err := newClientServer(lifecycle, failingServerTestConnector(), disabledEdgeController{}, noServiceControl{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}
	server.wake = power
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = transport.Serve(w, r, server.Handle) }))
	defer httpServer.Close()
	conn, err := transport.DialOnce(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), transport.DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // test cleanup
	probe := wakeRoundTrip(t, conn, protocol.Control{Type: protocol.TypeWakeProbe, WakeGrant: &grant})
	if probe.Type != protocol.TypeWakeProbed || !probe.WakeCanSend || probe.WakeState != wake.Down {
		t.Fatalf("probe = %+v", probe)
	}
	if sends.Load() != 0 {
		t.Fatal("read-only probe transmitted wake")
	}
	for range 2 {
		response := wakeRoundTrip(t, conn, protocol.Control{Type: protocol.TypeWakeSend, WakeGrant: &grant})
		if response.Type != protocol.TypeWakeSent {
			t.Fatalf("send = %+v", response)
		}
	}
	if sends.Load() != 1 {
		t.Fatalf("wake transmissions = %d", sends.Load())
	}
	allowed := true
	refused := wakeRoundTrip(t, conn, protocol.Control{Type: protocol.TypeWakeConfigure, WakeAllowed: &allowed})
	if refused.Type != protocol.TypeError || !strings.Contains(refused.Message, "local daemon socket") {
		t.Fatalf("remote permission change = %+v", refused)
	}
	revoked, err := authority.SetAllowed(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	learned := wakeRoundTrip(t, conn, protocol.Control{Type: protocol.TypeWakeRemember, WakeGrant: &revoked})
	if learned.Type != protocol.TypeWakeRemembered {
		t.Fatalf("revocation = %+v", learned)
	}
	refused = wakeRoundTrip(t, conn, protocol.Control{Type: protocol.TypeWakeSend, WakeGrant: &grant})
	if refused.Type != protocol.TypeError || sends.Load() != 1 {
		t.Fatalf("replayed permission = %+v, sends %d", refused, sends.Load())
	}
	forged := grant
	forged.Signature = make([]byte, ed25519.SignatureSize)
	refused = wakeRoundTrip(t, conn, protocol.Control{Type: protocol.TypeWakeSend, WakeGrant: &forged})
	if refused.Type != protocol.TypeError || sends.Load() != 1 {
		t.Fatal("forged permission transmitted wake")
	}
}

func testWakeController(t *testing.T) (*wakeController, *wake.Authority, *atomic.Int32) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nic := wake.NIC{MAC: "02:00:00:00:00:02", Address: "192.168.20.2", Prefix: "192.168.20.0/24", GatewayMAC: "02:00:00:00:00:01"}
	authority, err := wake.NewAuthorityWithOptions(t.TempDir(), key, wake.AuthorityOptions{
		Discover: func(context.Context) (wake.NIC, error) { return nic, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	cache, err := wake.NewCache(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	sends := &atomic.Int32{}
	sender, err := wake.NewSenderWithOptions(stateDir, wake.SenderOptions{
		Discover: func(context.Context) (wake.NIC, error) {
			local := nic
			local.Address = "192.168.20.3"
			return local, nil
		},
		Observe:  func(context.Context, wake.NIC) (wake.State, error) { return wake.Down, nil },
		Transmit: func(context.Context, wake.NIC, string) error { sends.Add(1); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &wakeController{authority: authority, sender: sender, cache: cache, slots: make(chan struct{}, 16), changed: make(chan struct{}, 1)}, authority, sends
}

func TestWakePermissionUsesRealLocalDaemonSocket(t *testing.T) {
	stateDir := t.TempDir()
	for restart := range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- run(ctx, Config{StateDir: stateDir}, runOptions{now: time.Now, bootID: func() string { return "wake-test-boot" },
				discoverSelf: func(context.Context) (tailnet.Peer, error) { return tailnet.Peer{}, nil }, reconcileInterval: time.Hour})
		}()
		client := dialUnixRuntime(t, SocketPath(stateDir))
		info := wakeRoundTrip(t, client, protocol.Control{Type: protocol.TypeHostInfo})
		if info.Host == nil || info.Host.Wake == nil || info.Host.Wake.Enabled {
			t.Fatalf("restart %d default permission = %+v", restart, info.Host)
		}
		if err := wake.ValidateGrant(*info.Host.Wake, time.Now()); err != nil {
			t.Fatal(err)
		}
		denied := false
		response := wakeRoundTrip(t, client, protocol.Control{Type: protocol.TypeWakeConfigure, WakeAllowed: &denied})
		if response.Type != protocol.TypeWakeConfigured || response.WakeGrant == nil || response.WakeGrant.Enabled {
			t.Fatalf("local configure = %+v", response)
		}
		_ = client.Close()
		cancel()
		if err := waitRuntime(t, done); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWakeLocalConsentIsAdvertisedWithoutSending(t *testing.T) {
	power, _, sends := testWakeController(t)
	allowed := true
	ctx := context.WithValue(context.Background(), localClientKey{}, true)
	response, handled, err := power.HandleControl(ctx, protocol.Control{Type: protocol.TypeWakeConfigure, RequestID: "allow", WakeAllowed: &allowed})
	if err != nil || !handled || response.WakeGrant == nil || !response.WakeGrant.Enabled {
		t.Fatalf("allow = %+v, %v", response, err)
	}
	if info := power.info(); info == nil || info.TargetID != response.WakeGrant.TargetID || !info.Enabled {
		t.Fatalf("advertised = %+v", info)
	}
	if sends.Load() != 0 {
		t.Fatal("granting permission sent a wake packet")
	}
}

func wakeRoundTrip(t *testing.T, conn transport.Conn, request protocol.Control) protocol.Control {
	t.Helper()
	request.RequestID = fmt.Sprintf("%s-%d", request.Type, time.Now().UnixNano())
	if err := conn.WriteFrame(controlFrame(t, request)); err != nil {
		t.Fatal(err)
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	response := decodeServerControl(t, frame)
	if response.RequestID != request.RequestID {
		t.Fatal("wake response lost request correlation")
	}
	return response
}
