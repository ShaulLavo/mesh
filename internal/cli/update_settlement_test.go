package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updategate"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestLocalFleetCancelSettlesTerminalInstallation(t *testing.T) {
	for _, phase := range []updateinstall.Phase{updateinstall.Failed, updateinstall.RolledBack, updateinstall.RollbackFailed, updateinstall.Granted} {
		t.Run(string(phase), func(t *testing.T) {
			_, status := localApprovalFixture(t)
			store, err := update.OpenStore(status.Settings.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			local := update.LocalHost(status.Settings.StateDir, status.Request.TargetID)
			run, err := store.Start(local.ID, scopedUpdateFleet("local", []update.Host{local}), status.Request.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			run, err = store.Change(run.ID, func(r *update.Run) error {
				r.CoordinatorBootstrap = true
				r.Targets[0].Generation = status.Request.Generation
				r.Targets[0].State, r.Targets[0].Grant = update.Failed, true
				r.Targets[0].Problem = "persisted installation failure"
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			status.Request.ID, status.Phase, status.Error = run.ID, phase, "persisted installation failure"
			status.Settings.ClientOnly = false
			status.Settings.Service = updateinstall.ServiceSpec{Kind: "systemd", Name: "mesh-test.service"}
			writeApprovalJournal(t, status)
			if err := updategate.Set(status.Settings.StateDir, run.ID); err != nil {
				t.Fatal(err)
			}
			_, _, err = executeCommand(t, Dependencies{}, "update", "cancel", run.ID, "--json")
			if code, ok := StatusCode(err); !ok || code != 1 {
				t.Fatalf("cancel exit = %v", err)
			}
			gateErr := updategate.Check(status.Settings.StateDir)
			if phase == updateinstall.Granted {
				if !errors.Is(gateErr, updategate.ErrUpdating) {
					t.Fatalf("active gate released: %v", gateErr)
				}
			} else if gateErr != nil {
				t.Fatalf("local terminal cancellation retained gate: %v", gateErr)
			}
			durable, err := updateinstall.Read(status.Settings.StateDir)
			if err != nil || durable.Phase != phase || durable.Error != status.Error {
				t.Fatalf("cancellation changed receipt: %+v %v", durable, err)
			}
			cancelled, err := store.Read(run.ID)
			if err != nil || !cancelled.Cancel {
				t.Fatalf("fleet cancellation lost: %+v %v", cancelled, err)
			}
		})
	}
}

func TestLocalStatusReportsTerminalGateWithoutReleasingIt(t *testing.T) {
	_, status := localApprovalFixture(t)
	status.Phase = updateinstall.RollbackFailed
	writeApprovalJournal(t, status)
	if err := updategate.Set(status.Settings.StateDir, status.Request.ID); err != nil {
		t.Fatal(err)
	}
	_, diagnostic, _ := executeCommand(t, Dependencies{}, "update", "status")
	if !strings.Contains(diagnostic, "rollback_failed") || !strings.Contains(diagnostic, "mesh update cancel "+status.Request.ID) {
		t.Fatalf("terminal gate guidance missing: %q", diagnostic)
	}
	if err := updategate.Check(status.Settings.StateDir); !errors.Is(err, updategate.ErrUpdating) {
		t.Fatalf("status released gate: %v", err)
	}
}
