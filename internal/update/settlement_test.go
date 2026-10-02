package update

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/updategate"
	"github.com/shaul/mesh/internal/updateinstall"
)

type terminalInstallationCaller struct {
	engine        *updateinstall.Engine
	health        updateinstall.Health
	cancellations int
}

func (c *terminalInstallationCaller) Call(ctx context.Context, _ Host, action string, input, output any) error {
	status, err := c.engine.Read()
	if err != nil {
		return fmt.Errorf("fixture transport installation read: %w", err)
	}
	switch action {
	case "info":
		*output.(*Info) = Info{Health: c.health, Installation: &status}
		return nil
	case "install-cancel":
		c.cancellations++
		operation := input.(Operation)
		status, err = c.engine.Cancel(ctx, operation.ID, operation.Generation)
		*output.(*updateinstall.Status) = status
		if err != nil {
			return fmt.Errorf("fixture transport installation cancellation: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unexpected fixture transport action %s", action)
	}
}

func TestCancelledFleetSettlesTerminalInstallationReceipt(t *testing.T) {
	for _, phase := range []updateinstall.Phase{updateinstall.RolledBack, updateinstall.Failed, updateinstall.RollbackFailed} {
		t.Run(string(phase), func(t *testing.T) {
			fleet := testFleet(t, 1)
			host := fleet.Members[0]
			store, err := OpenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.Start(host.ID, fleet, testManifest())
			if err != nil {
				t.Fatal(err)
			}
			run, err = store.Change(run.ID, func(r *Run) error {
				r.Cached = true
				r.Targets[0].Generation, r.Targets[0].State, r.Targets[0].Grant = 1, Failed, true
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			settings := updateinstall.Settings{StateDir: dir, Executable: filepath.Join(dir, "mesh"), CacheDir: filepath.Join(dir, "cache"), ClientOnly: true}
			status := updateinstall.Status{Schema: 1, Phase: phase, Settings: settings, Error: "original terminal failure", Request: updateinstall.Request{ID: run.ID, TargetID: host.ID, Generation: 1, Manifest: run.Release}}
			data, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.MkdirAll(filepath.Join(dir, "update"), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(dir, "update", "installation.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if err = updategate.Set(dir, run.ID); err != nil {
				t.Fatal(err)
			}
			engine, err := updateinstall.New(settings.Config())
			if err != nil {
				t.Fatal(err)
			}
			remote := &terminalInstallationCaller{engine: engine, health: updateinstall.Health{HostID: host.ID}}
			coordinator := Coordinator{ID: host.ID, Store: store, Remote: remote}
			if _, err = store.Cancel(run.ID); err != nil {
				t.Fatal(err)
			}
			if err = coordinator.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
			if remote.cancellations != 1 {
				t.Fatalf("terminal cancellation dispatches = %d", remote.cancellations)
			}
			if err = updategate.Check(dir); err != nil {
				t.Fatalf("fleet cancellation retained gate: %v", err)
			}
			receipt, err := engine.Read()
			if err != nil || receipt.Phase != phase || receipt.Error != status.Error {
				t.Fatalf("terminal receipt changed: %+v %v", receipt, err)
			}
			final, err := store.Read(run.ID)
			if err != nil || final.Targets[0].Grant || final.Targets[0].State != Failed {
				t.Fatalf("fleet terminal receipt = %+v %v", final, err)
			}
		})
	}
}
