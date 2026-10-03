package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

func TestDestinationNamePublishedAfterCommitAndRetryCoalesces(t *testing.T) {
	directory := t.TempDir()
	host, _, err := identity.LoadOrCreate(directory)
	if err != nil {
		t.Fatal(err)
	}
	names, err := machinename.Open(t.Context(), directory, host.ID, "source-pc")
	if err != nil {
		t.Fatal(err)
	}
	broker := newStateBroker(2, time.Now)
	life, err := newLifecycle(lifecycleConfig{
		Catalog: &lifecycleTestCatalog{}, Names: names, NameChanged: broker.hostChanged,
		Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
			return nil, errors.New("naming must not contact a worker")
		}),
		Host: storage.Host{ID: storage.HostID(host.ID), MeshIdentity: host.ID}, SessionsDir: directory,
	})
	if err != nil {
		t.Fatal(err)
	}
	broker.hostChanged(life.declaredHostInfo())
	sub, initial, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicHost}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.unsubscribe(sub)
	if initial.Host == nil || initial.Host.MachineName != "source-pc" || initial.Host.NameRevision != 1 {
		t.Fatal("host snapshot lacks the destination's declaration")
	}
	initial.Host.MachineName = "caller-mutation"
	request := protocol.Control{Type: protocol.TypeHostRename, RequestID: "rename-one", Rename: &protocol.HostRename{
		TargetID: host.ID, MachineName: "destination-pc", ExpectedRevision: 1,
	}}
	response, handled, err := life.HandleControl(t.Context(), request)
	if err != nil || !handled || response.Type != protocol.TypeHostRenamed || response.Host.NameRevision != 2 {
		t.Fatalf("rename handled=%v type=%q error=%v", handled, response.Type, err)
	}
	reopened, err := machinename.Open(t.Context(), directory, host.ID, "different-os-host")
	if err != nil || reopened.Current().MachineName != response.Host.MachineName {
		t.Fatalf("reply preceded persisted name: %v", err)
	}
	messages := broker.take(sub)
	if len(messages) != 1 || messages[0].StateEvent.Kind != "host.changed" || messages[0].StateEvent.Payload.Host.MachineName != "destination-pc" {
		t.Fatal("committed name did not reach state watch")
	}
	messages[0].StateEvent.Payload.Host.MachineName = "caller-mutation"
	if _, _, err := life.HandleControl(t.Context(), request); err != nil || len(broker.take(sub)) != 0 {
		t.Fatal("retry published a second name event")
	}
	stale := life.declaredHostInfo()
	stale.NameRevision = 1
	stale.MachineName = "stale-pc"
	broker.hostChanged(stale)
	reconnected, snapshot, err := broker.subscribe(protocol.StateWatch{Topics: []string{protocol.TopicHost}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.unsubscribe(reconnected)
	if snapshot.Host == nil || snapshot.Host.NameRevision != 2 || snapshot.Host.MachineName != "destination-pc" {
		t.Fatal("reconnect lost newer authoritative declaration")
	}
}

func TestHostRenameErrorsAreStructuredAndWatchReadOnly(t *testing.T) {
	if clientErrorCode(machinename.ErrRevision) != "host.name_revision" || clientErrorCode(machinename.ErrTarget) != "host.name_target" {
		t.Fatal("rename errors lost their wire codes")
	}
	server := &clientServer{}
	if _, err := server.stateReadControl(t.Context(), protocol.Control{Type: protocol.TypeHostRename}); !errors.Is(err, errWatchMode) {
		t.Fatal("watch connection admitted name mutation")
	}
}
