package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestUpdateOutputSeparatesRebootInterruptedSessions(t *testing.T) {
	var output bytes.Buffer
	problem := update.Target{Host: update.Host{MachineName: "server"}, State: update.Updated,
		InterruptedWorkers: []updateinstall.Worker{{ID: "7K3D", PID: 123}}}
	if err := printUpdateTargets(&output, []update.Target{problem}, release.Manifest{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "1 sessions interrupted by a machine reboot") || strings.Contains(output.String(), "preserved") {
		t.Fatalf("reboot interruption misreported: %s", &output)
	}
}

func TestUpdateObservationWaitsThroughAnAuthorizedTargetsRestart(t *testing.T) {
	restarting := update.Run{Targets: []update.Target{{State: update.Offline, Grant: true}}}
	if updateObservationSettled(restarting) {
		t.Fatal("stopped watching while the authorized target restarted onto the release")
	}
	unreachable := update.Run{Targets: []update.Target{{State: update.Offline}}}
	if !updateObservationSettled(unreachable) {
		t.Fatal("kept waiting on a target that was offline before it was authorized")
	}
}

func TestDeclaredUpdateTargetsUseRetainedOwnerClaimsWithoutChangingOperation(t *testing.T) {
	first := namedDestination(t)
	second := namedDestinationPeer(t)
	for _, fixture := range []*namedDestinationFixture{first, second} {
		fixture.host.MachineName, fixture.host.NameRevision = "garden", 1
		if err := saveNamedTestHost(t, fixture.host); err != nil {
			t.Fatal(err)
		}
	}
	targets := []update.Target{
		{Host: update.Host{ID: first.host.ID, MachineName: "obsolete-viewer"}, State: update.Offline},
		{Host: update.Host{ID: second.host.ID}, State: update.Updated},
		{Host: update.Host{ID: "unknown-exact-id", MachineName: "invented"}, State: update.Pending},
	}
	original := append([]update.Target(nil), targets...)
	before, err := json.Marshal(targets)
	if err != nil {
		t.Fatal(err)
	}
	shown, err := declaredUpdateTargets(t.Context(), targets)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range shown[:2] {
		if !strings.Contains(target.Host.Label(), "garden [") || !strings.Contains(target.Host.Label(), "conflict") || !strings.Contains(target.Host.Label(), "last known name") {
			t.Fatalf("retained declaration status: %q", target.Host.Label())
		}
	}
	if shown[2].Host.Label() != "unknown-exact-id" {
		t.Fatalf("unknown target: %q", shown[2].Host.Label())
	}
	if !reflect.DeepEqual(targets, original) {
		t.Fatal("presentation changed original operation")
	}
	after, err := json.Marshal(shown)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("human declaration overlay changed operational JSON")
	}
	var out bytes.Buffer
	run := update.Run{ID: "fixture-run", Targets: targets}
	if err := printDeclaredUpdateRun(t.Context(), &out, run, false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "last known name") || strings.Contains(out.String(), "obsolete-viewer") {
		t.Fatalf("status uses persisted viewer label: %s", &out)
	}
}

func TestStructuredUpdateOutputNeedsNoNamingConfiguration(t *testing.T) {
	namedDestination(t)
	invalid := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(invalid, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_CONFIG_DIR", invalid)
	var out bytes.Buffer
	run := update.Run{ID: "fixture-run", Targets: []update.Target{{Host: update.Host{ID: "exact-id", MachineName: "ephemeral"}}}}
	if err := printDeclaredUpdateRun(t.Context(), &out, run, true, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "exact-id") || strings.Contains(out.String(), "ephemeral") {
		t.Fatalf("structured identity record: %s", &out)
	}
}
