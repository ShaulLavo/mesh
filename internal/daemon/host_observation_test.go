package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/transport"
)

func TestHostObservationRenewsUnchangedOwnerWithoutRename(t *testing.T) {
	directory := t.TempDir()
	owner, _, err := identity.LoadOrCreate(directory)
	if err != nil {
		t.Fatal(err)
	}
	names, err := machinename.Open(t.Context(), directory, owner.ID, "fixture-owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	broker := newStateBroker(1, func() time.Time { return now })
	life, err := newLifecycle(lifecycleConfig{
		Catalog: &lifecycleTestCatalog{}, Names: names, NameChanged: broker.hostChanged,
		Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
			return nil, errors.New("name observation must not contact a worker")
		}),
		Host: storage.Host{ID: storage.HostID(owner.ID), MeshIdentity: owner.ID}, SessionsDir: directory,
	})
	if err != nil {
		t.Fatal(err)
	}
	broker.hostChanged(life.declaredHostInfo())
	sub, initial, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicHost, protocol.TopicMetrics}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.unsubscribe(sub)
	if initial.Host == nil || initial.Current[protocol.TopicHost].AgeMillis != 0 || initial.Current[protocol.TopicHost].Failing {
		t.Fatal("known-good owner observation is not visible in the snapshot")
	}
	durable := names.Current()
	now = now.Add(31 * time.Second)
	broker.metricsChanged(hostmetrics.Snapshot{})
	messages := broker.take(sub)
	if len(messages) != 1 || messages[0].StateCurrent.Sections[protocol.TopicHost].AgeMillis != 31000 {
		t.Fatal("metrics renewed retained owner observation")
	}
	broker.hostChanged(life.declaredHostInfo())
	if events := broker.take(sub); len(events) != 0 {
		t.Fatalf("unchanged owner observation manufactured a rename: %+v", events)
	}
	observation := broker.current(sub).Sections[protocol.TopicHost]
	if observation.AgeMillis != 0 || observation.Failing {
		t.Fatalf("unchanged owner observation was not renewed: %+v", observation)
	}
	broker.observe(protocol.TopicHost, errors.New("fixture source failed"))
	if !broker.current(sub).Sections[protocol.TopicHost].Failing {
		t.Fatal("owner observation failure was hidden")
	}
	broker.take(sub)
	broker.hostChanged(life.declaredHostInfo())
	messages = broker.take(sub)
	if len(messages) != 1 || messages[0].Type != protocol.TypeStateCurrent || messages[0].StateCurrent.Sections[protocol.TopicHost].Failing {
		t.Fatal("owner observation recovery did not clear failure without a rename")
	}
	now = now.Add(31 * time.Second)
	if broker.current(sub).Sections[protocol.TopicHost].AgeMillis != 31000 {
		t.Fatal("retained owner state never becomes stale")
	}
	stored, err := machinename.Open(t.Context(), directory, owner.ID, "different-initial-name")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Current() != durable || names.Current() != durable {
		t.Fatal("observation altered the durable owner declaration")
	}
}

func TestRunRenewsHostObservationOnOwnerReconciliation(t *testing.T) {
	stateDir := compactSocketTempDir(t)
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC).UnixMilli())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, Config{StateDir: stateDir}, runOptions{
			now: func() time.Time { return time.UnixMilli(clock.Load()) }, bootID: func() string { return "fixture-boot" },
			discoverSelf:      func(context.Context) (tailnet.Peer, error) { return tailnet.Peer{}, nil },
			reconcileInterval: 10 * time.Millisecond,
		})
	}()
	defer func() {
		cancel()
		if err := waitRuntime(t, done); err != nil {
			t.Error(err)
		}
	}()
	conn := dialUnixRuntime(t, SocketPath(stateDir))
	defer func() { _ = conn.Close() }()
	request := protocol.Control{Type: protocol.TypeStateWatch, RequestID: "fixture-watch", Watch: &protocol.StateWatch{Topics: []string{protocol.TopicHost}}}
	initial := watchContractRequest(t, conn, request)
	if initial.StateSnapshot == nil || initial.StateSnapshot.Host == nil || initial.StateSnapshot.Current[protocol.TopicHost].AgeMillis != 0 {
		t.Fatal("native watch known-good host observation is missing")
	}
	clock.Add(31000)
	deadline := time.Now().Add(time.Second)
	for {
		reconnected := dialUnixRuntime(t, SocketPath(stateDir))
		next := watchContractRequest(t, reconnected, request)
		_ = reconnected.Close()
		if next.StateSnapshot == nil || next.StateSnapshot.Host == nil {
			t.Fatal("reconnected native watch lost the owner declaration")
		}
		if next.StateSnapshot.Host.MachineName != initial.StateSnapshot.Host.MachineName || next.StateSnapshot.Host.NameRevision != initial.StateSnapshot.Host.NameRevision {
			t.Fatal("owner reconciliation changed the durable declaration")
		}
		observation := next.StateSnapshot.Current[protocol.TopicHost]
		if observation.AgeMillis == 0 && !observation.Failing {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("owner reconciliation did not renew native watch observation: %+v", observation)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHostObservationRefusesConflictReplayAndForeignOwner(t *testing.T) {
	for _, change := range []string{"conflict", "replay", "foreign-id-equal", "foreign-id-newer", "foreign-pin-equal", "foreign-pin-newer"} {
		t.Run(change, func(t *testing.T) {
			now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
			broker := newStateBroker(1, func() time.Time { return now })
			original := protocol.HostInfo{ID: "owner-id", MeshIdentity: "owner-pin", MachineName: "fixture-owner", NameRevision: 2}
			broker.hostChanged(original)
			now = now.Add(time.Minute)
			next := original
			switch change {
			case "conflict":
				next.MachineName = "conflicting-name"
			case "replay":
				next.NameRevision = 1
			case "foreign-id-equal", "foreign-id-newer":
				next.ID = "foreign-id"
			case "foreign-pin-equal", "foreign-pin-newer":
				next.MeshIdentity = "foreign-pin"
			}
			if change == "foreign-id-newer" || change == "foreign-pin-newer" {
				next.NameRevision++
			}
			broker.hostChanged(next)
			sub, snapshot, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicHost}})
			if err != nil {
				t.Fatal(err)
			}
			defer broker.unsubscribe(sub)
			if *snapshot.Host != original || snapshot.Current[protocol.TopicHost].AgeMillis != 60000 {
				t.Fatal("refused owner observation changed the declaration or renewed its age")
			}
		})
	}
}
