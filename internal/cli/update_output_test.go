package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestUpdateOutputSeparatesRebootInterruptedSessions(t *testing.T) {
	var output bytes.Buffer
	problem := update.Target{Host: update.Host{Alias: "server"}, State: update.Updated,
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
