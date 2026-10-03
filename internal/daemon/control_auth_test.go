package daemon

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
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
