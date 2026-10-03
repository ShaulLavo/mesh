package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

func TestHostRenameCannotHealRevokedConnection(t *testing.T) {
	state := t.TempDir()
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	actor, key, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	names, err := machinename.Open(t.Context(), state, host.ID, "source-pc")
	if err != nil {
		t.Fatal(err)
	}
	life, err := newLifecycle(lifecycleConfig{Names: names, Catalog: &lifecycleTestCatalog{},
		Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
			return nil, fmt.Errorf("naming fixture contacted a worker")
		}), Host: storage.Host{ID: storage.HostID(host.ID), MeshIdentity: host.ID}, SessionsDir: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.ApproveDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	auth, err := controlAuthentication(state)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, func(ctx context.Context, conn transport.Conn) error {
			return serveNamingFixture(ctx, conn, life)
		})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	opts := transport.DialOptions{Auth: &transport.Authentication{Key: key, ExpectedIdentity: host.ID}}
	prior, err := transport.DialOnce(ctx, server.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer prior.Close() //nolint:errcheck // fixture cleanup
	if err := identity.RevokeDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	if err := identity.ApproveDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	request := protocol.Control{Type: protocol.TypeHostRename, RequestID: "retired-rename", Rename: &protocol.HostRename{
		TargetID: host.ID, MachineName: "destination-pc", ExpectedRevision: 1,
	}}
	payload, err := request.Encode()
	if err != nil {
		t.Fatal(err)
	}
	legacy, legacyErr := transport.DialOnce(ctx, server.URL, transport.DialOptions{})
	if legacyErr == nil {
		_ = legacy.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
		_, _ = legacy.ReadFrame()
		_ = legacy.Close()
		t.Fatal("unauthenticated naming socket admitted")
	}
	_ = prior.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload})
	if _, err := prior.ReadFrame(); err == nil {
		t.Fatal("reapproval healed the old naming connection")
	}
	if names.Current().Revision != 1 {
		t.Fatal("revoked connection changed the destination name")
	}
	fresh, err := transport.DialOnce(ctx, server.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close() //nolint:errcheck // fixture cleanup
	if err := fresh.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	frame, err := fresh.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil || response.Type != protocol.TypeHostRenamed || names.Current().Revision != 2 {
		t.Fatalf("fresh grant failed to rename: %v", err)
	}
}

func serveNamingFixture(ctx context.Context, conn transport.Conn, life *lifecycle) error {
	for {
		frame, err := conn.ReadFrame()
		if err != nil {
			return fmt.Errorf("naming fixture read: %w", err)
		}
		request, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return fmt.Errorf("naming fixture decode: %w", err)
		}
		response, handled, err := life.HandleControl(ctx, request)
		if err != nil {
			return err
		}
		if !handled {
			return fmt.Errorf("naming fixture declined control")
		}
		payload, err := response.Encode()
		if err != nil {
			return fmt.Errorf("naming fixture encode: %w", err)
		}
		if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
			return fmt.Errorf("naming fixture write: %w", err)
		}
	}
}
