package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
)

func policyControlFixture(t *testing.T) (*updateController, update.Client, update.Client, update.Host, update.Host) {
	t.Helper()
	state := compactSocketTempDir(t)
	host, key, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	other, otherKey, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := update.Trust(t.Context(), state, other.ID, true); err != nil {
		t.Fatal(err)
	}
	controller, err := newUpdateController(t.Context(), state, host.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	listener, port := newTCPListener(t, "127.0.0.1:0")
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	config := ListenerConfig{StateDir: state, TailnetAddrs: []string{"127.0.0.1"}, TailnetPort: port, WebSocketPath: "/mesh"}
	done := runRuntime(t, ctx, config, func(ctx context.Context, conn transport.Conn) error {
		return servePolicyControls(ctx, conn, controller)
	}, []net.Listener{listener}...)
	t.Cleanup(func() {
		cancel()
		if err := waitRuntime(t, done); err != nil {
			t.Error(err)
		}
	})
	ready := dialUnixRuntime(t, SocketPath(state))
	_ = ready.Close()
	local := update.LocalHost(state, host.ID)
	remote := local
	remote.Endpoint = "ws://" + listener.Addr().String() + "/mesh"
	return controller, update.Client{ID: host.ID, Key: key}, update.Client{ID: other.ID, Key: otherKey}, local, remote
}

func servePolicyControls(ctx context.Context, conn transport.Conn, controller *updateController) error {
	for {
		frame, err := conn.ReadFrame()
		if err != nil {
			return fmt.Errorf("read policy fixture frame: %w", err)
		}
		request, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return fmt.Errorf("decode policy fixture control: %w", err)
		}
		response, handled, err := controller.HandleControl(ctx, request)
		if err != nil || !handled {
			return fmt.Errorf("fixture update dispatch: %w", err)
		}
		payload, err := response.Encode()
		if err != nil {
			return fmt.Errorf("encode policy fixture reply: %w", err)
		}
		if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
			return fmt.Errorf("write policy fixture reply: %w", err)
		}
	}
}

func TestHostPolicyLocalOwnerAndNetworkRejection(t *testing.T) {
	controller, owner, administrator, local, remote := policyControlFixture(t)
	change := update.HostPolicyChange{ExpectedRevision: 0, Policy: update.HostPolicy{Mode: update.HostAutomatic, After: []string{administrator.ID}, Soak: time.Hour}}
	var policy update.HostPolicy
	if err := owner.Call(t.Context(), local, "host-policy.set", change, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Revision != 1 || policy.Mode != update.HostAutomatic || policy.Soak != time.Hour {
		t.Fatalf("local policy not saved: %+v", policy)
	}
	change.ExpectedRevision, change.Policy.Mode = 1, update.HostManual
	assertPolicyRefused(t, administrator, local, change)
	assertPolicyRefused(t, administrator, remote, change)
	assertPolicyRefused(t, owner, remote, change)
	if err := administrator.Call(t.Context(), remote, "host-policy.show", nil, &policy); err != nil {
		t.Fatalf("read-only policy refused: %v", err)
	}
	if policy.Revision != 1 || policy.Mode != update.HostAutomatic {
		t.Fatalf("rejected transport changed policy: %+v", policy)
	}
	if err := owner.Call(t.Context(), local, "host-policy.set", change, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Revision != 2 || policy.Mode != update.HostManual {
		t.Fatalf("off not persisted: %+v", policy)
	}
	saved, err := controller.coordinator.Store.HostPolicy(t.Context())
	if err != nil || saved.Revision != policy.Revision || saved.Mode != policy.Mode {
		t.Fatalf("reply differs from authority store: %+v %v", saved, err)
	}
	if err := owner.Call(t.Context(), local, "host-policy.set", change, &policy); err == nil || !strings.Contains(err.Error(), "revision changed") {
		t.Fatalf("stale policy overwrote off: %v", err)
	}
}

func assertPolicyRefused(t *testing.T, client update.Client, host update.Host, change update.HostPolicyChange) {
	t.Helper()
	var remote *update.RemoteError
	err := client.Call(t.Context(), host, "host-policy.set", change, nil)
	if !errors.As(err, &remote) || !strings.Contains(err.Error(), "local Unix socket") {
		t.Fatalf("policy mutation was not locally refused: %v", err)
	}
}

func TestHostPolicyConcurrentRevisionAllowsOneWriter(t *testing.T) {
	controller, owner, _, local, _ := policyControlFixture(t)
	var writers sync.WaitGroup
	results := make(chan error, 2)
	for _, mode := range []update.HostMode{update.HostAutomatic, update.HostManual} {
		writers.Go(func() {
			change := update.HostPolicyChange{Policy: update.HostPolicy{Mode: mode}}
			results <- owner.Call(t.Context(), local, "host-policy.set", change, nil)
		})
	}
	writers.Wait()
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("concurrent policy writers: %v, %v", first, second)
	}
	policy, err := controller.coordinator.Store.HostPolicy(t.Context())
	if err != nil || policy.Revision != 1 {
		t.Fatalf("revision advanced more than once: %+v %v", policy, err)
	}
}
