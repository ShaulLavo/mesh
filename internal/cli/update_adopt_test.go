package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
)

func TestAdoptFleetKeepsPCPolicyIndependentOfMac(t *testing.T) {
	state, local := setupUpdateCLI(t)
	var peers []update.Host
	for range 3 {
		host, _, err := identity.LoadOrCreate(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		peers = append(peers, update.Host{ID: host.ID, Endpoint: "wss://fixture.invalid/mesh", Platform: release.CurrentPlatform()})
	}
	fleet := update.Fleet{Version: 1, Name: "adoption", Revision: 1, Members: append(peers, local)}
	path := filepath.Join(t.TempDir(), "fleet.json")
	saveUpdateTestFleet(t, path, fleet)
	options := adoptOptions{version: "v0.1.400", fleet: path, last: "LOCAL", canaries: []string{peers[0].ID, peers[1].ID}, lead: 30 * time.Minute}
	for i, peer := range peers {
		options.ssh = append(options.ssh, peer.ID+"="+[]string{"pi", "vps", "mac"}[i])
	}
	start, err := fleetAdoptionInput(state, options, []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	if len(start.Require) != 3 || len(start.Policy.After) != 2 || len(start.Remotes[2].Require) != 2 || start.Notice.Lead != options.lead {
		t.Fatalf("incorrect initial/ordinary ordering: %+v", start)
	}
	for _, id := range start.Policy.After {
		if id == peers[2].ID {
			t.Fatal("sleeping Mac holds ordinary later PC policy")
		}
	}
	options.ssh = options.ssh[:2]
	if _, err := fleetAdoptionInput(state, options, []string{"true"}); err == nil || !strings.Contains(err.Error(), "--ssh") {
		t.Fatalf("missing system SSH alias was not a precise blocker: %v", err)
	}
}

func TestAdoptLocalInputBindsReleaseIdentityAndOperation(t *testing.T) {
	_, host := setupUpdateCLI(t)
	input := update.AdoptionLocalInput{Schema: 1, ID: strings.Repeat("a", 32), Pin: strings.Repeat("b", 64), Version: "v0.1.400", Expected: host.ID, Policy: update.HostPolicy{Mode: update.HostAutomatic}}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	options := adoptOptions{local: true, version: input.Version}
	start, err := localAdoptionInput(bytes.NewReader(data), options, nil)
	if err != nil || start.ID != input.ID || start.Expected != host.ID || start.Pin != input.Pin {
		t.Fatalf("local input lost pin: %+v %v", start, err)
	}
	options.version = "v0.1.401"
	if _, err := localAdoptionInput(bytes.NewReader(data), options, nil); err == nil {
		t.Fatal("different release accepted")
	}
	options.version, options.notice = input.Version, true
	if _, err := localAdoptionInput(bytes.NewReader(data), options, []string{"true"}); err == nil {
		t.Fatal("remote record gained a local notice command")
	}
}

func TestAdoptStatusDoesNotCreateMissingStateOrIdentity(t *testing.T) {
	state := filepath.Join(t.TempDir(), "uninitialized")
	t.Setenv("MESH_STATE_DIR", state)
	if err := writeAdoptionStatus(t.Context(), new(bytes.Buffer), true); err == nil {
		t.Fatal("missing adoption reported success")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("read-only adoption status initialized state")
	}
}

func TestAdoptMalformedInputPrecedesReleaseNetworkOrMutation(t *testing.T) {
	state, _ := setupUpdateCLI(t)
	command := updateAdoptCommand(&updateOptions{json: true})
	command.SetIn(strings.NewReader(`{"schema":1}`))
	command.SetArgs([]string{"start", "--local", "--release", "v0.1.400"})
	err := command.ExecuteContext(t.Context())
	if err == nil {
		t.Fatal("malformed adoption input accepted")
	}
	if _, err := os.Stat(filepath.Join(state, "updates")); !os.IsNotExist(err) {
		t.Fatal("malformed input initialized update state")
	}
}
