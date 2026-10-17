package cli

import (
	"context"
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
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	var calls atomic.Int32
	dial := func(ctx context.Context, _ HostRecord) (transport.Conn, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	started := time.Now()
	_, err := resolveArgumentWithRefresh(ctx, "unknown", []HostRecord{f.host, other.host}, dial)
	if err == nil || time.Since(started) > time.Second || calls.Load() != 2 {
		t.Fatalf("lookup exceeded shared deadline: %v, elapsed %s, calls %d", err, time.Since(started), calls.Load())
	}
}
