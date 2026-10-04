package updateinstall

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func bridgeInstallationFixture(t *testing.T) (helperUpgradeFixture, Health, release.Manifest) {
	t.Helper()
	f := newHelperUpgradeFixture(t, "systemd", "v0.3.0")
	f.commit(t, "v0.1.0", "")
	status, err := Read(f.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	status.Phase = RolledBack
	status.Settings.ClientOnly = false
	status.Settings.Service = ServiceSpec{Kind: "systemd", Name: "mesh.service"}
	writeBridgeJournal(t, f.cfg.StateDir, status)
	tool := filepath.Join(filepath.Dir(f.commands), "tools", "systemctl")
	data := []byte("#!/bin/sh\n[ \"$*\" = '--user show mesh.service --property=KillMode --value' ] || exit 99\nprintf '%s\\n' \"$*\" >> \"$MESH_HELPER_TEST_COMMANDS\"\nprintf 'process\\n'\n")
	if err := os.WriteFile(tool, data, 0755); err != nil { //nolint:gosec // test-only service manager rejects every mutating command
		t.Fatal(err)
	}
	health := Health{HostID: status.Request.TargetID, Build: release.Build{Version: "v0.1.0", Digest: status.Request.Manifest.Artifacts[0].BinarySHA256,
		Platform: release.CurrentPlatform(), StateVersion: 7, WorkerProtocol: 1, UpdateProtocol: 1}}
	hop := status.Request.Manifest
	hop.Version = "v0.2.0"
	hop.Artifacts = append([]release.Artifact(nil), hop.Artifacts...)
	for i := range hop.Artifacts {
		hop.Artifacts[i].BinarySHA256 = strings.Repeat("e", 64)
	}
	hop.Compatibility.Transitions = []release.Transition{{Platform: health.Build.Platform, FromDigest: health.Build.Digest, ToDigest: strings.Repeat("e", 64), Proof: strings.Repeat("f", 64)}}
	return f, health, hop
}

func writeBridgeJournal(t *testing.T, stateDir string, status Status) {
	t.Helper()
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(journalPath(stateDir), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReviewBridgeChecksReadOnlyOwnershipAndRecoveredHelper(t *testing.T) {
	f, health, hop := bridgeInstallationFixture(t)
	before := f.snapshot(t)
	probes := 0
	probe := func(ctx context.Context, installed HelperInstallation) (int, error) {
		probes++
		if installed != f.prior {
			return 0, errors.New("helper image differs from fixture's running image")
		}
		return 123, release.VerifyExecutable(ctx, installed.Executable, installed.Digest)
	}
	if err := ReviewBridge(t.Context(), f.cfg.StateDir, f.cfg.Executable, health, hop, probe); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, f.snapshot(t)) || probes != 1 {
		t.Fatal("bridge review changed ownership or skipped executing-helper verification")
	}
	commands, err := os.ReadFile(f.commands) //nolint:gosec // fixture command log
	if err != nil || string(commands) != "--user show mesh.service --property=KillMode --value\n" {
		t.Fatalf("bridge service checks = %q, %v", commands, err)
	}
}

func TestReviewBridgeRejectsUnsafeReadiness(t *testing.T) {
	for _, scenario := range []string{"rollback failed", "activation pending", "client only", "wrong owner", "wrong daemon digest", "unknown worker", "unfinished helper promotion", "old helper", "stopped helper", "unsafe service"} {
		t.Run(scenario, func(t *testing.T) {
			f, health, hop := bridgeInstallationFixture(t)
			mutateBridgeInstallation(t, scenario, f, &health, &hop)
			before := f.snapshot(t)
			probe := func(context.Context, HelperInstallation) (int, error) {
				if scenario == "stopped helper" {
					return 0, errors.New("fixture helper service is stopped")
				}
				return 123, nil
			}
			if err := ReviewBridge(t.Context(), f.cfg.StateDir, f.cfg.Executable, health, hop, probe); err == nil {
				t.Fatalf("unsafe %s readiness accepted", scenario)
			}
			if !reflect.DeepEqual(before, f.snapshot(t)) {
				t.Fatal("rejected bridge mutated installation")
			}
		})
	}
}

func mutateBridgeInstallation(t *testing.T, scenario string, f helperUpgradeFixture, health *Health, hop *release.Manifest) {
	t.Helper()
	status, err := Read(f.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	switch scenario {
	case "rollback failed":
		status.Phase = RollbackFailed
	case "activation pending":
		status.Phase = Activating
	case "client only":
		status.Settings.ClientOnly = true
	case "wrong owner":
		status.Request.TargetID = "another-host"
	case "wrong daemon digest":
		health.Build.Digest = strings.Repeat("a", 64)
	case "unknown worker":
		health.Workers = []Worker{{ID: "retained", Protocol: 0}}
	case "unfinished helper promotion":
		link := filepath.Join(transactionDir(f.cfg.StateDir), "helper", "current")
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f.cfg.Executable, link); err != nil {
			t.Fatal(err)
		}
	case "old helper":
		hop.Version = "v0.4.0"
	case "unsafe service":
		status.Settings.Service.Kind = "unmanaged"
	}
	writeBridgeJournal(t, f.cfg.StateDir, status)
}
