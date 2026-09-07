package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestAbsentCoordinatorPreviewAddsExplicitSetupWithoutInstalling(t *testing.T) {
	stateDir, local, _, file := firstCoordinatorFleet(t)
	client, _ := updateTestRelease(t)
	caller := updateCallFunc(func(_ context.Context, host update.Host, action string, _, output any) error {
		if action != "info" {
			t.Fatalf("preview mutation %s", action)
		}
		if host.ID == local.ID {
			return os.ErrNotExist
		}
		*output.(*update.Info) = update.Info{Health: updateinstall.Health{HostID: host.ID}}
		return nil
	})
	stdout, _, err := executeCommand(t, Dependencies{UpdateRelease: client, UpdateCaller: caller}, "update", "--fleet", file, "--check", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var preview updatePreview
	if err = json.Unmarshal([]byte(stdout), &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.CoordinatorSetup || !preview.CoordinatorBootstrap || !preview.CoordinatorAdded || len(preview.Fleet.Members) != 2 {
		t.Fatalf("absent daemon omitted setup: %+v", preview)
	}
	if _, err = os.Stat(filepath.Join(stateDir, "updates", "runs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview created an approved operation")
	}
}

func TestAbsentCoordinatorPersistsApprovalBeforeHelperInstallation(t *testing.T) {
	stateDir, local, _, file := firstCoordinatorFleet(t)
	client, _ := updateTestRelease(t)
	installed := false
	caller := updateCallFunc(func(_ context.Context, host update.Host, action string, input, output any) error {
		if action == "info" {
			if host.ID == local.ID {
				return os.ErrNotExist
			}
			*output.(*update.Info) = update.Info{Health: updateinstall.Health{HostID: host.ID}}
			return nil
		}
		if action != "status" || !installed {
			t.Fatalf("unapproved mutation %s", action)
		}
		store, err := update.OpenStore(stateDir)
		if err != nil {
			return err
		}
		run, err := store.Read(input.(update.Operation).ID)
		if err != nil {
			return err
		}
		for i := range run.Targets {
			run.Targets[i].State = update.Updated
			run.Targets[i].Grant = false
		}
		*output.(*update.Run) = run
		return nil
	})
	setup := func(_ context.Context, dir string) error {
		_, run, err := coordinatorSetupRun(dir)
		if err != nil {
			return err
		}
		if len(run.Targets) != 2 || run.SetupBuild == nil || run.SetupExecutable == "" || run.SetupCacheDir == "" {
			t.Fatalf("helper installation preceded durable scope: %+v", run)
		}
		installed = true
		return nil
	}
	_, _, err := executeCommand(t, Dependencies{UpdateRelease: client, UpdateCaller: caller, UpdateCoordinatorSetup: setup}, "update", "--fleet", file, "--yes", "--json")
	if err != nil || !installed {
		t.Fatalf("setup did not resume original operation: %v", err)
	}
}

func coordinatorSetupFixture(t *testing.T) (string, *update.Store, update.Run, updateinstall.Health) {
	t.Helper()
	stateDir, local := setupUpdateCLI(t)
	manifest := updateTestManifest()
	build := release.Build{Version: "v0.1.0", Commit: strings.Repeat("d", 40), Digest: strings.Repeat("d", 64), Platform: release.CurrentPlatform(), StateVersion: 1, WorkerProtocol: 1, UpdateProtocol: 1}
	manifest.Compatibility.Transitions = append(manifest.Compatibility.Transitions, release.Transition{FromDigest: build.Digest, ToDigest: strings.Repeat("c", 64), Platform: build.Platform, Proof: strings.Repeat("e", 64)})
	store, err := update.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Start(local.ID, scopedUpdateFleet("setup", []update.Host{local}), manifest)
	if err != nil {
		t.Fatal(err)
	}
	run, err = store.Change(run.ID, func(run *update.Run) error {
		run.CoordinatorSetup = true
		run.CoordinatorBootstrap = true
		run.Cached = true
		run.SetupBuild = &build
		run.SetupExecutable = filepath.Join(stateDir, "mesh")
		run.SetupCacheDir = filepath.Join(stateDir, "cache")
		run.Targets[0].Grant = true
		run.Targets[0].State = update.Granted
		run.Targets[0].Generation = 1
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return stateDir, store, run, updateinstall.Health{HostID: local.ID, Build: build}
}

func TestOlderCoordinatorSetupPersistsReceiptBeforeDaemonCanReconcile(t *testing.T) {
	stateDir, store, run, health := coordinatorSetupFixture(t)
	c := &update.Coordinator{ID: run.Coordinator, Store: store}
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	var receipt updateinstall.Status
	bootstrap := func(_ context.Context, request updatebootstrap.Request, _ updatebootstrap.Config) (updateinstall.Status, error) {
		saved, err := store.Read(request.ID)
		if err != nil {
			return receipt, err
		}
		if !saved.CoordinatorSetup || !saved.Targets[0].Grant {
			t.Fatal("setup authority cleared before installation receipt")
		}
		receipt = updateinstall.Status{Schema: 1, Phase: updateinstall.Staged, Settings: updateinstall.Settings{StateDir: stateDir}, Request: updateinstall.Request{ID: request.ID, TargetID: request.TargetID, Generation: request.Generation, Manifest: request.Manifest, Current: health.Build}}
		writeApprovalJournal(t, receipt)
		return receipt, nil
	}
	if err := advanceCoordinatorSetup(context.Background(), stateDir, store, run, health, bootstrap); err != nil {
		t.Fatal(err)
	}
	c.Remote = updateCallFunc(func(_ context.Context, _ update.Host, action string, _, output any) error {
		if action == "info" {
			*output.(*update.Info) = update.Info{Health: health, Installation: &receipt}
			return nil
		}
		if action == "grant" {
			receipt.Phase = updateinstall.Granted
			*output.(*updateinstall.Status) = receipt
			return nil
		}
		t.Fatalf("unexpected action %s", action)
		return nil
	})
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Read(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CoordinatorSetup || saved.Stopped || receipt.Phase != updateinstall.Granted {
		t.Fatalf("setup daemon lost its original update: %+v phase=%s", saved, receipt.Phase)
	}
}

func TestSetupReentryReusesOriginalOperationAndExactBinary(t *testing.T) {
	stateDir, _, run, health := coordinatorSetupFixture(t)
	health.Build.Digest = strings.Repeat("c", 64)
	health.Build.Version = run.Release.Version
	store, err := update.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	_, resumed, err := coordinatorSetupRun(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	bootstrap := func(context.Context, updatebootstrap.Request, updatebootstrap.Config) (updateinstall.Status, error) {
		called = true
		return updateinstall.Status{}, nil
	}
	if err = advanceCoordinatorSetup(context.Background(), stateDir, store, resumed, health, bootstrap); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Read(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if called || saved.CoordinatorSetup || saved.Targets[0].Grant || saved.Targets[0].State != update.Updated || saved.ID != run.ID {
		t.Fatalf("same-release setup replaced or lost approved binary: %+v", saved)
	}
}

func TestCancelledAndStoppedSetupsDoNotBlockNewApproval(t *testing.T) {
	stateDir, store, old, _ := coordinatorSetupFixture(t)
	if _, err := store.Cancel(old.ID); err != nil {
		t.Fatal(err)
	}
	manifest := old.Release
	manifest.Version = "v0.3.0"
	newRun, err := store.Start(old.Coordinator, old.Fleet, manifest)
	if err != nil {
		t.Fatal(err)
	}
	newRun, err = store.Change(newRun.ID, func(run *update.Run) error {
		run.CoordinatorSetup = true
		run.CoordinatorBootstrap = true
		run.Targets[0].Grant = true
		run.Targets[0].Generation = 2
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = cancelPendingCoordinatorSetups(stateDir); err != nil {
		t.Fatal(err)
	}
	_, selected, err := coordinatorSetupRun(stateDir)
	if err != nil || selected.ID != newRun.ID {
		t.Fatalf("cancelled setup blocked new approval: %s %v", selected.ID, err)
	}
	old, err = store.Read(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.CoordinatorSetup || old.Targets[0].Grant || old.Targets[0].State != update.Cancelled {
		t.Fatal("cancelled setup still had authority")
	}
}

func TestCoordinatorServicePreservesExistingUnitAndCustomPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.service")
	original := []byte("[Service]\nExecStart=/custom/mesh daemon --tailnet-port=9000\nEnvironment=CUSTOM=yes\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishCoordinatorService(path, []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(path) //nolint:gosec // explicit service fixture within this test's temporary directory
	if err != nil || string(actual) != string(original) {
		t.Fatal("existing service configuration was overwritten")
	}
	_, _, run, _ := coordinatorSetupFixture(t)
	run.SetupExecutable = "/work/app with spaces/mesh"
	service, err := renderCoordinatorService("linux", "/work/custom state", run)
	if err != nil || !strings.Contains(service, "ExecStart=\"/work/app with spaces/mesh\" daemon") || !strings.Contains(service, "MESH_STATE_DIR=/work/custom state") || !strings.Contains(service, "KillMode=process") {
		t.Fatalf("custom path rendering: %s %v", service, err)
	}
}

func TestCoordinatorSetupRetainsExistingUserLaunchDomain(t *testing.T) {
	root := t.TempDir()
	script := []byte("#!/bin/sh\ncase \"$2\" in user/*/dev.shaulavo.mesh) exit 0 ;; gui/*/dev.shaulavo.mesh) exit 1 ;; gui/*|user/*) exit 0 ;; esac\nexit 1\n")
	if err := os.WriteFile(filepath.Join(root, "launchctl"), script, 0700); err != nil { //nolint:gosec // isolated executable service-manager fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	domain, err := coordinatorLaunchDomain(context.Background())
	if err != nil || domain != fmt.Sprintf("user/%d", os.Getuid()) {
		t.Fatalf("existing service moved domains: %s %v", domain, err)
	}
}

func TestPackageCoordinatorMigrationIsRequiredBeforeChangingServices(t *testing.T) {
	_, _, run, health := coordinatorSetupFixture(t)
	if err := coordinatorSetupMigration("/opt/homebrew/Caskroom/mesh/v0.1.0/mesh", health.Build, run.Release); err == nil || !strings.Contains(err.Error(), "one-time installation migration") {
		t.Fatalf("missing migration preview: %v", err)
	}
	health.Build.Digest = strings.Repeat("c", 64)
	if err := coordinatorSetupMigration("/opt/homebrew/Caskroom/mesh/v0.2.0/mesh", health.Build, run.Release); err != nil {
		t.Fatalf("same-release service setup rewrites no package payload: %v", err)
	}
}
