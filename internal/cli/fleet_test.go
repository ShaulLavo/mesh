package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/fleetshare"
	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/protocol"
)

func TestFleetTrustRequiresExplicitConsentAndPinnedDestination(t *testing.T) {
	f := namedDestination(t)
	if err := SaveHost(f.host); err != nil {
		t.Fatal(err)
	}
	directory, err := fleetDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeCommand(t, Dependencies{DialControl: dialControlHost}, "fleet", "trust", f.host.ID); err == nil {
		t.Fatal("sharing policy changed without consent")
	}
	peers, err := fleetshare.Peers(directory)
	if err != nil || len(peers) != 0 {
		t.Fatalf("unapproved policy changed: %+v %v", peers, err)
	}
	if _, _, err := executeCommand(t, Dependencies{DialControl: dialControlHost}, "fleet", "trust", f.host.ID, "--yes"); err != nil {
		t.Fatal(err)
	}
	peers, err = fleetshare.Peers(directory)
	if err != nil || len(peers) != 1 || peers[0].Identity != f.host.ID || peers[0].Endpoint != f.host.Endpoint {
		t.Fatalf("trusted pinned controller: %+v %v", peers, err)
	}
	if _, _, err := executeCommand(t, Dependencies{}, "fleet", "untrust", f.host.ID, "--yes"); err != nil {
		t.Fatal(err)
	}
	peers, err = fleetshare.Peers(directory)
	if err != nil || len(peers) != 0 {
		t.Fatalf("removed controller retained: %+v %v", peers, err)
	}
}

func TestAddGrantsOnlyExplicitConnectionSharingDevices(t *testing.T) {
	controller := namedDestination(t)
	other := namedDestinationPeer(t)
	for _, host := range []HostRecord{controller.host, other.host} {
		if err := SaveHost(host); err != nil {
			t.Fatal(err)
		}
	}
	directory, err := fleetDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if err := fleetshare.Enroll(directory, fleetshare.Peer{Identity: controller.host.ID, Endpoint: controller.host.Endpoint}); err != nil {
		t.Fatal(err)
	}
	added := namedDestinationPeer(t)
	added.host.MachineName, added.host.NameRevision = "new-server", 1
	called := false
	bootstrap := func(_ context.Context, request AddRequest) (BootstrapResult, error) {
		called = true
		if !reflect.DeepEqual(request.AdditionalDeviceIdentities, []string{controller.host.ID}) || !request.AllowRoot {
			t.Errorf("add inferred devices from the address book: %+v", request)
		}
		return BootstrapResult{Host: added.host, AuthenticatedIdentity: added.host.ID}, nil
	}
	if _, _, err := executeCommand(t, Dependencies{Bootstrap: bootstrap}, "add", "root@new-server", "--allow-root"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("bootstrap was not called")
	}
	peers, err := fleetshare.Peers(directory)
	if err != nil || len(peers) != 1 || peers[0].Identity != controller.host.ID {
		t.Fatalf("adding a target enrolled it as a controller: %+v %v", peers, err)
	}
}

func TestSharedConnectionsPreserveLocalPins(t *testing.T) {
	local := HostRecord{ID: "host", MeshIdentity: "local-pin", Endpoint: "ws://127.0.0.1:1/mesh"}
	shared := []protocol.FleetHost{{ID: "host", MeshIdentity: "remote-pin", Endpoint: "ws://127.0.0.1:2/mesh"}, {ID: "new", MeshIdentity: "new-pin", Endpoint: "ws://127.0.0.1:3/mesh"}, {ID: "self", MeshIdentity: "self", Endpoint: "ws://127.0.0.1:4/mesh"}}
	hosts, err := mergeSharedHosts([]HostRecord{local}, shared, "self")
	if err != nil || len(hosts) != 2 || !reflect.DeepEqual(hosts[0], local) || hosts[1].ID != "new" {
		t.Fatalf("shared connections replaced local pin: %+v %v", hosts, err)
	}
}

func TestRootCommandsCannotBecomeMachineNames(t *testing.T) {
	root := NewCommand(Dependencies{})
	for _, command := range root.Commands() {
		if _, err := machinename.Normalize(command.Name()); err == nil {
			t.Errorf("root command %q is available as a machine name", command.Name())
		}
	}
}

func TestRootRefreshDetectsCompetingOwnerNames(t *testing.T) {
	first := namedDestination(t)
	second := namedDestinationPeer(t)
	for _, host := range []HostRecord{first.host, second.host} {
		if err := SaveHost(host); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := executeCommand(t, Dependencies{DialControl: dialControlHost}, "destination", "--resume")
	if err == nil || !strings.Contains(err.Error(), "shared by hosts") || first.operations.Load()+second.operations.Load() != 0 {
		t.Fatalf("discovered competing name executed a command: %v", err)
	}
}
