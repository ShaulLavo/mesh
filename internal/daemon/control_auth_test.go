package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
)

func TestRemoteControlRejectsLegacyBeforeSessionDispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), runtimeTestTimeout)
	defer cancel()
	listener, port := newTCPListener(t, "127.0.0.1:0")
	cfg := ListenerConfig{StateDir: compactSocketTempDir(t), TailnetAddrs: []string{"127.0.0.1"}, TailnetPort: port, WebSocketPath: "/mesh"}
	var dispatched atomic.Int32
	handler := func(_ context.Context, conn transport.Conn) error {
		frame, err := conn.ReadFrame()
		if err != nil {
			return fmt.Errorf("fixture read: %w", err)
		}
		dispatched.Add(1)
		if err := conn.WriteFrame(frame); err != nil {
			return fmt.Errorf("fixture echo: %w", err)
		}
		return nil
	}
	done := runRuntime(t, ctx, cfg, handler, []net.Listener{listener}...)
	conn, err := transport.DialOnce(ctx, "ws://"+listener.Addr().String()+"/mesh", transport.DialOptions{})
	if err == nil {
		payload, encodeErr := (protocol.Control{Type: protocol.TypeCreate, RequestID: "unauthenticated"}).Encode()
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		_ = conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
		_, _ = conn.ReadFrame()
		_ = conn.Close()
	}
	if dispatched.Load() != 0 {
		t.Fatalf("unauthenticated session command reached handler: dispatched=%d", dispatched.Load())
	}
	local := dialUnixRuntime(t, filepath.Join(cfg.StateDir, daemonSocketName))
	assertControlEcho(t, local)
	_ = local.Close()
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func runtimeDialOptions(t *testing.T, stateDir string) transport.DialOptions {
	t.Helper()
	host, key, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.ApproveDevice(stateDir, host.ID); err != nil {
		t.Fatal(err)
	}
	return transport.DialOptions{Auth: &transport.Authentication{Key: key, ExpectedIdentity: host.ID}}
}

func TestUpdateAdministratorHasNoImplicitSessionGrant(t *testing.T) {
	state := t.TempDir()
	actor, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := update.Trust(state, actor.ID, true); err != nil {
		t.Fatal(err)
	}
	auth, err := controlAuthentication(state)
	if err != nil {
		t.Fatal(err)
	}
	control := func(kind string) protocol.Frame {
		payload, err := (protocol.Control{Type: kind}).Encode()
		if err != nil {
			t.Fatal(err)
		}
		return protocol.Frame{Kind: protocol.KindControl, Payload: payload}
	}
	if !auth.Authorize(actor.ID) || !auth.Admit(actor.ID, control(update.ControlType)) {
		t.Fatal("existing signed updater authority lost access")
	}
	if auth.Admit(actor.ID, control(protocol.TypeCreate)) || auth.Admit(actor.ID, protocol.Frame{Kind: protocol.KindInput}) {
		t.Fatal("update authority gained session or terminal access")
	}
	if err := identity.ApproveDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	if !auth.Admit(actor.ID, control(protocol.TypeCreate)) {
		t.Fatal("explicit device approval did not grant session access")
	}
	retainedDeviceGrant := auth.Retain(actor.ID)
	if err := identity.RevokeDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	if retainedDeviceGrant() || !auth.Retain(actor.ID)() {
		t.Fatal("revoked device socket survived through its separate updater authority")
	}
	if auth.Admit(actor.ID, control(protocol.TypeCreate)) || !auth.Admit(actor.ID, control(update.ControlType)) {
		t.Fatal("device revocation changed separate update authority or retained session access")
	}
}

func TestDeniedAuthenticationReleasesControlSlot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), runtimeTestTimeout)
	defer cancel()
	listener, port := newTCPListener(t, "127.0.0.1:0")
	cfg := ListenerConfig{StateDir: compactSocketTempDir(t), TailnetAddrs: []string{"127.0.0.1"}, TailnetPort: port, WebSocketPath: "/mesh", TailnetConnectionLimit: 1}
	done := runRuntime(t, ctx, cfg, echoControlFrames, listener)
	approved := runtimeDialOptions(t, cfg.StateDir)
	_, unknown, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "ws://" + listener.Addr().String() + "/mesh"
	denied, err := transport.DialOnce(ctx, endpoint, transport.DialOptions{Auth: &transport.Authentication{Key: unknown, ExpectedIdentity: approved.Auth.ExpectedIdentity}})
	if denied != nil || !errors.Is(err, transport.ErrAuthentication) {
		t.Fatalf("unknown device connection=%v, error=%v", denied, err)
	}
	var conn transport.Conn
	for range 20 {
		conn, err = transport.DialOnce(ctx, endpoint, approved)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("denied handshake retained reserved slot: %v", err)
	}
	assertControlEcho(t, conn)
	_ = conn.Close()
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonIdentityDoesNotImplicitlyGrantRemoteSessions(t *testing.T) {
	state := t.TempDir()
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := controlAuthentication(state)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := (protocol.Control{Type: protocol.TypeCreate}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	frame := protocol.Frame{Kind: protocol.KindControl, Payload: payload}
	if auth.Admit(host.ID, frame) {
		t.Fatal("daemon key bypassed current device grants")
	}
	if err := identity.ApproveDevice(state, host.ID); err != nil {
		t.Fatal(err)
	}
	if !auth.Admit(host.ID, frame) {
		t.Fatal("explicit self-device grant rejected")
	}
	if err := identity.RevokeDevice(state, host.ID); err != nil {
		t.Fatal(err)
	}
	if auth.Admit(host.ID, frame) {
		t.Fatal("revoked self-device grant retained session authority")
	}
}
