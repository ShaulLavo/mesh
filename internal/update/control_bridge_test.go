package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestAuthenticatedCoordinatorUsesOnlySignedUpdatesWithOlderHost(t *testing.T) {
	target, key := testIdentity(t)
	actor, actorKey := testIdentity(t)
	var calls, frames atomic.Int32
	authority := &Authority{StateDir: t.TempDir(), ID: target, Key: key, Handle: func(_ context.Context, action string, _ json.RawMessage) (any, error) {
		if action != "info" {
			return nil, fmt.Errorf("unexpected update action %q", action)
		}
		calls.Add(1)
		return "healthy", nil
	}}
	if err := Trust(authority.StateDir, actor, true); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The bridge fixture keeps the pre-authentication WebSocket framing.
		_ = transport.Serve(w, r, func(ctx context.Context, conn transport.Conn) error {
			for {
				frame, err := conn.ReadFrame()
				if err != nil {
					return fmt.Errorf("legacy fixture read: %w", err)
				}
				request, err := protocol.DecodeControl(frame.Payload)
				if err != nil {
					return fmt.Errorf("legacy fixture decode: %w", err)
				}
				if request.Type != ControlType {
					return fmt.Errorf("unsigned legacy control %q", request.Type)
				}
				frames.Add(1)
				response, handled, err := authority.HandleControl(ctx, request)
				if err != nil {
					return fmt.Errorf("legacy authority: %w", err)
				}
				if !handled {
					return fmt.Errorf("legacy authority did not handle update")
				}
				payload, err := response.Encode()
				if err != nil {
					return fmt.Errorf("legacy response encode: %w", err)
				}
				if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
					return fmt.Errorf("legacy response write: %w", err)
				}
			}
		})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	host := Host{ID: target, Alias: "older", Endpoint: server.URL, Platform: testFleet(t, 1).Members[0].Platform}
	// Host validation accepts WebSocket schemes, while httptest exposes HTTP.
	host.Endpoint = "ws" + host.Endpoint[4:]
	client := Client{ID: actor, Key: actorKey}
	var output string
	if err := client.Call(ctx, host, "info", nil, &output); err != nil {
		t.Fatal(err)
	}
	if output != "healthy" || calls.Load() != 1 || frames.Load() != 2 {
		t.Fatalf("output=%q calls=%d frames=%d", output, calls.Load(), frames.Load())
	}
	before := frames.Load()
	if conn, err := client.dial(ctx, host); err == nil {
		_ = conn.Close()
		t.Fatal("ordinary controls entered legacy bridge")
	}
	if frames.Load() != before {
		t.Fatal("ordinary dial sent a legacy control")
	}
	unknown, unknownKey := testIdentity(t)
	if err := (Client{ID: unknown, Key: unknownKey}).Call(ctx, host, "info", nil, nil); err == nil {
		t.Fatal("unenrolled updater admitted")
	}
	if calls.Load() != 1 {
		t.Fatal("unenrolled updater dispatched an action")
	}
}
