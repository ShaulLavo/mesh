package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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
	if !auth.Authorize(actor.ID) || !auth.Bind(actor.ID).Allows(control(update.ControlType)) {
		t.Fatal("existing signed updater authority lost access")
	}
	if auth.Bind(actor.ID).Allows(control(protocol.TypeCreate)) || auth.Bind(actor.ID).Allows(protocol.Frame{Kind: protocol.KindInput}) {
		t.Fatal("update authority gained session or terminal access")
	}
	if err := identity.ApproveDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	if !auth.Bind(actor.ID).Allows(control(protocol.TypeCreate)) {
		t.Fatal("explicit device approval did not grant session access")
	}
	retainedDeviceGrant := auth.Bind(actor.ID).Current
	if err := identity.RevokeDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	if retainedDeviceGrant() || !auth.Bind(actor.ID).Current() {
		t.Fatal("revoked device socket survived through its separate updater authority")
	}
	if auth.Bind(actor.ID).Allows(control(protocol.TypeCreate)) || !auth.Bind(actor.ID).Allows(control(update.ControlType)) {
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
	if auth.Bind(host.ID).Allows(frame) {
		t.Fatal("daemon key bypassed current device grants")
	}
	if err := identity.ApproveDevice(state, host.ID); err != nil {
		t.Fatal(err)
	}
	if !auth.Bind(host.ID).Allows(frame) {
		t.Fatal("explicit self-device grant rejected")
	}
	if err := identity.RevokeDevice(state, host.ID); err != nil {
		t.Fatal(err)
	}
	if auth.Bind(host.ID).Allows(frame) {
		t.Fatal("revoked self-device grant retained session authority")
	}
}

func TestRevokedDeviceCanReconnectOnlyThroughSignedUpdater(t *testing.T) {
	state := t.TempDir()
	host, key, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	actor, actorKey, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.ApproveDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	if err := update.Trust(state, actor.ID, true); err != nil {
		t.Fatal(err)
	}
	auth, err := controlAuthentication(state)
	if err != nil {
		t.Fatal(err)
	}
	var actions atomic.Int32
	authority := &update.Authority{StateDir: state, ID: host.ID, Key: key, Handle: func(_ context.Context, action string, _ json.RawMessage) (any, error) {
		if action != "info" {
			return nil, fmt.Errorf("unexpected fixture update action %q", action)
		}
		actions.Add(1)
		return "healthy", nil
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, func(ctx context.Context, conn transport.Conn) error {
			for {
				frame, err := conn.ReadFrame()
				if err != nil {
					return fmt.Errorf("signed fixture read: %w", err)
				}
				request, err := protocol.DecodeControl(frame.Payload)
				if err != nil {
					return fmt.Errorf("signed fixture decode: %w", err)
				}
				response, handled, err := authority.HandleControl(ctx, request)
				if err != nil {
					return fmt.Errorf("signed fixture authority: %w", err)
				}
				if !handled {
					return fmt.Errorf("signed fixture authority declined the control")
				}
				payload, err := response.Encode()
				if err != nil {
					return fmt.Errorf("signed fixture encode: %w", err)
				}
				if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
					return fmt.Errorf("signed fixture write: %w", err)
				}
			}
		})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	clientAuth := &transport.Authentication{Key: actorKey, ExpectedIdentity: host.ID}
	full, err := transport.DialOnce(ctx, server.URL, transport.DialOptions{Auth: clientAuth})
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close() //nolint:errcheck // fixture cleanup
	if err := identity.RevokeDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := full.ReadFrame(); err == nil {
		t.Fatal("existing full-device socket retained independent updater authority")
	}
	denied, err := transport.DialOnce(ctx, server.URL, transport.DialOptions{Auth: clientAuth})
	if denied != nil || !errors.Is(err, transport.ErrAuthentication) {
		t.Fatalf("ordinary update-only dial: connection=%v error=%v", denied, err)
	}
	var result string
	target := update.Host{ID: host.ID, Alias: "fixture", Endpoint: "ws" + server.URL[4:]}
	if err := (update.Client{ID: actor.ID, Key: actorKey}).Call(ctx, target, "info", nil, &result); err != nil {
		t.Fatal(err)
	}
	if result != "healthy" || actions.Load() != 1 {
		t.Fatalf("fresh signed update result=%q actions=%d", result, actions.Load())
	}
}
