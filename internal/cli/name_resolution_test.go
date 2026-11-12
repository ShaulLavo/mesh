package cli

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestRootFindsRenamedPinnedHostWithoutListingFirst(t *testing.T) {
	f := namedDestination(t)
	if err := SaveHost(f.host); err != nil {
		t.Fatal(err)
	}
	conn, _, err := openVerifiedHostInfo(t.Context(), f.host, dialControlHost)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if _, _, err := f.names.Rename(t.Context(), f.host.ID, "upcloud-4", 1); err != nil {
		t.Fatal(err)
	}
	_, _, err = executeCommand(t, Dependencies{DialHost: dialControlHost, DialControl: dialControlHost}, "upcloud-4", "--resume")
	if err == nil || !strings.Contains(err.Error(), "upcloud-4 has no active sessions") || f.operations.Load() != 1 {
		t.Fatalf("new name did not reach the pinned destination: %v, session queries %d", err, f.operations.Load())
	}
}

func TestRootDoesNotAdoptUnverifiedNameDuringLookup(t *testing.T) {
	f := namedDestination(t)
	if err := SaveHost(f.host); err != nil {
		t.Fatal(err)
	}
	f.reported.Store(&protocol.HostInfo{ID: "another-host", MeshIdentity: f.host.ID, MachineName: "forged", NameRevision: 99})
	_, _, err := executeCommand(t, Dependencies{DialControl: dialControlHost}, "forged", "--resume")
	if err == nil || !strings.Contains(err.Error(), "neither a declared machine name") || f.operations.Load() != 0 {
		t.Fatalf("unverified name resolved: %v, session queries %d", err, f.operations.Load())
	}
}

func TestKnownTargetsDoNotRefreshOtherHosts(t *testing.T) {
	f := namedDestination(t)
	var calls atomic.Int32
	dial := func(context.Context, HostRecord) (transport.Conn, error) {
		calls.Add(1)
		t.Error("resolved target refreshed host names")
		return nil, context.Canceled
	}
	for _, value := range []string{f.host.ID, "7K3D"} {
		if _, err := resolveArgumentWithRefresh(t.Context(), value, []HostRecord{f.host}, dial); err != nil {
			t.Fatalf("known target %s: %v", value, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("known target caused discovery")
	}
}

func TestNameRefreshUsesOneDeadlineForOfflineHosts(t *testing.T) {
	f := namedDestination(t)
	other := namedDestinationPeer(t)
	for _, host := range []HostRecord{f.host, other.host} {
		if err := SaveHost(host); err != nil {
			t.Fatal(err)
		}
	}
	// Registry refresh may precede dialing; cancel only after both offline
	// hosts expose the lookup budget they were given.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	contexts := make(chan context.Context, 2)
	var dial HostDialer = func(ctx context.Context, _ HostRecord) (transport.Conn, error) {
		contexts <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	}
	started := time.Now()
	expired := time.After(time.Second)
	done := make(chan error, 1)
	go func() {
		defer close(done)
		_, err := resolveArgumentWithRefresh(ctx, "unknown", []HostRecord{f.host, other.host}, dial)
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("name refresh did not stop after cancellation")
		}
	})
	first := receiveNameRefreshContext(t, contexts, expired)
	second := receiveNameRefreshContext(t, contexts, expired)
	deadline, bounded := first.Deadline()
	otherDeadline, otherBounded := second.Deadline()
	if !bounded || !otherBounded || !deadline.Equal(otherDeadline) {
		t.Fatalf("offline hosts did not share one bounded context: %v, %v", first, second)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || time.Since(started) > time.Second {
			t.Fatalf("lookup exceeded shared deadline: %v, elapsed %s", err, time.Since(started))
		}
	case <-expired:
		t.Fatal("name refresh did not return within its shared bound")
	}
}

func receiveNameRefreshContext(t *testing.T, contexts <-chan context.Context, expired <-chan time.Time) context.Context {
	t.Helper()
	select {
	case ctx := <-contexts:
		return ctx
	case <-expired:
		t.Fatal("offline name refresh did not start both dials")
		return nil
	}
}
