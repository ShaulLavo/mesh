package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

func firstCoordinatorFleet(t *testing.T) (string, update.Host, update.Fleet, string) {
	t.Helper()
	stateDir, local := setupUpdateCLI(t)
	remote, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fleet := scopedUpdateFleet("remote only", []update.Host{{ID: remote.ID, Alias: "laptop", Endpoint: "ws://laptop.invalid/mesh"}})
	file := filepath.Join(t.TempDir(), "fleet.json")
	if err := update.SaveFleet(file, fleet); err != nil {
		t.Fatal(err)
	}
	return stateDir, local, fleet, file
}

func legacyCoordinatorInfo(local update.Host, host update.Host, output any) error {
	if host.ID == local.ID {
		return &update.RemoteError{Problem: `daemon: unknown control "update.control"`}
	}
	*output.(*update.Info) = update.Info{Health: updateinstall.Health{HostID: host.ID}}
	return nil
}

func TestFirstCoordinatorPreviewIncludesAdditionalScopeWithoutApproval(t *testing.T) {
	stateDir, local, _, file := firstCoordinatorFleet(t)
	client, _ := updateTestRelease(t)
	caller := updateCallFunc(func(_ context.Context, host update.Host, action string, _, output any) error {
		if action != "info" {
			t.Fatalf("preview mutation: %s", action)
		}
		return legacyCoordinatorInfo(local, host, output)
	})
	stdout, stderr, err := executeCommand(t, Dependencies{UpdateRelease: client, UpdateCaller: caller}, "update", "--fleet", file, "--check", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var preview updatePreview
	if err := json.Unmarshal([]byte(stdout), &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.CoordinatorBootstrap || !preview.CoordinatorAdded || len(preview.Fleet.Members) != 2 || len(preview.Targets) != 2 || stderr != "" {
		t.Fatalf("scope omitted: %+v, stderr %q", preview, stderr)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "updates", "runs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview persisted approval: %v", err)
	}
}

func TestFirstCoordinatorPersistsEntireApprovedRunBeforeActivation(t *testing.T) {
	stateDir, local, fleet, file := firstCoordinatorFleet(t)
	client, _ := updateTestRelease(t)
	var bootstrapped bool
	caller := updateCallFunc(func(_ context.Context, host update.Host, action string, input, output any) error {
		if action == "info" {
			return legacyCoordinatorInfo(local, host, output)
		}
		if action != "status" || !bootstrapped {
			t.Fatalf("unexpected coordinator action: %s", action)
		}
		store, err := update.OpenStore(stateDir)
		if err != nil {
			return err
		}
		run, err := store.Read(input.(update.Operation).ID)
		if err != nil {
			return err
		}
		for index := range run.Targets {
			run.Targets[index].State = update.Updated
			run.Targets[index].Grant = false
		}
		*output.(*update.Run) = run
		return nil
	})
	bootstrap := func(_ context.Context, request updatebootstrap.Request, config updatebootstrap.Config) (updateinstall.Status, error) {
		store, err := update.OpenStore(stateDir)
		if err != nil {
			return updateinstall.Status{}, err
		}
		run, err := store.Read(request.ID)
		if err != nil {
			return updateinstall.Status{}, err
		}
		index := coordinatorTargetIndex(run, local.ID)
		if !run.CoordinatorBootstrap || len(run.Targets) != 2 || index < 0 || !run.Targets[index].Grant || run.Targets[index].Generation != request.Generation || request.Generation == 0 || request.CoordinatorID != local.ID || run.ReleaseDigest != request.Manifest.Digest() {
			t.Fatalf("activation preceded durable intent: %+v, %+v", run, request)
		}
		if coordinatorTargetIndex(run, fleet.Members[0].ID) < 0 {
			t.Fatal("original scope disappeared")
		}
		if err := config.Enroll(stateDir, local.ID); err != nil {
			return updateinstall.Status{}, err
		}
		bootstrapped = true
		return updateinstall.Status{Phase: updateinstall.Granted, Request: updateinstall.Request{ID: request.ID, TargetID: request.TargetID, Generation: request.Generation, Manifest: request.Manifest}}, nil
	}
	stdout, stderr, err := executeCommand(t, Dependencies{UpdateRelease: client, UpdateCaller: caller, UpdateBootstrap: bootstrap}, "update", "--fleet", file, "--yes", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var run update.Run
	if err := json.Unmarshal([]byte(stdout), &run); err != nil {
		t.Fatal(err)
	}
	if !bootstrapped || len(run.Targets) != 2 || !run.Done() || stderr != "" {
		t.Fatalf("bootstrap did not resume saved fleet: %+v, stderr %q", run, stderr)
	}
}

func TestFirstCoordinatorBootstrapCannotOverwriteExplicitPolicy(t *testing.T) {
	stateDir, local, _, file := firstCoordinatorFleet(t)
	client, _ := updateTestRelease(t)
	if _, err := update.OpenStore(stateDir); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(stateDir, "updates", "administrators.json")
	original := []byte(`{"version":1,"keys":[]}`)
	if err := os.WriteFile(policy, original, 0o600); err != nil {
		t.Fatal(err)
	}
	caller := updateCallFunc(func(_ context.Context, host update.Host, action string, _, output any) error {
		if action != "info" {
			t.Fatalf("unexpected action: %s", action)
		}
		return legacyCoordinatorInfo(local, host, output)
	})
	bootstrap := func(_ context.Context, request updatebootstrap.Request, config updatebootstrap.Config) (updateinstall.Status, error) {
		return updateinstall.Status{}, config.Enroll(stateDir, request.CoordinatorID)
	}
	stdout, _, err := executeCommand(t, Dependencies{UpdateRelease: client, UpdateCaller: caller, UpdateBootstrap: bootstrap}, "update", "--fleet", file, "--yes", "--json")
	if code, ok := StatusCode(err); !ok || code != 1 {
		t.Fatalf("explicit policy overridden: %v", err)
	}
	after, err := os.ReadFile(policy) //nolint:gosec // Policy path belongs to this test’s temporary state directory.
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("bootstrap rewrote explicit policy")
	}
	if !strings.Contains(stdout, "policy already exists") {
		t.Fatalf("failure hidden: %s", stdout)
	}
}

func TestFirstCoordinatorHelperRequiresExactDurableAuthorization(t *testing.T) {
	for _, condition := range []string{"approved", "ordinary run", "cancelled", "wrong generation", "wrong release"} {
		t.Run(condition, func(t *testing.T) {
			engine, status := localApprovalFixture(t)
			status.Settings.ClientOnly = false
			store, err := update.OpenStore(status.Settings.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			local := update.LocalHost(status.Settings.StateDir, status.Request.TargetID)
			run, err := store.Start(local.ID, scopedUpdateFleet("local", []update.Host{local}), status.Request.Manifest)
			if err != nil {
				t.Fatal(err)
			}
			status.Request.ID = run.ID
			writeApprovalJournal(t, status)
			_, err = store.Change(run.ID, func(current *update.Run) error {
				current.CoordinatorBootstrap = condition != "ordinary run"
				current.Cancel = condition == "cancelled"
				current.Targets[0].Generation = status.Request.Generation
				current.Targets[0].Grant = true
				if condition == "wrong generation" {
					current.Targets[0].Generation++
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if condition == "wrong release" {
				status.Request.Manifest.Commit = strings.Repeat("d", 40)
			}
			granted, err := grantFirstCoordinator(context.Background(), engine, status)
			if condition == "approved" {
				if err != nil || granted.Phase != updateinstall.Granted {
					t.Fatalf("durable approval lost: %s, %v", granted.Phase, err)
				}
				return
			}
			if !errors.Is(err, updateinstall.ErrNoGrant) {
				t.Fatalf("unauthorized helper grant: %s, %v", granted.Phase, err)
			}
		})
	}
}

func TestFirstCoordinatorApprovalCannotAddUnreviewedHost(t *testing.T) {
	stateDir, local, fleet, _ := firstCoordinatorFleet(t)
	store, err := update.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Start(local.ID, fleet, updateTestManifest())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = approveFirstCoordinator(store, updateEnvironment{stateDir: stateDir, local: local}, run)
	if err == nil {
		t.Fatal("coordinator silently added after approval")
	}
	saved, err := store.Read(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CoordinatorBootstrap || saved.Membership != fleet.Digest() {
		t.Fatal("unapproved scope persisted")
	}
}

func TestFirstCoordinatorRetryKeepsApprovedRunAndUsesOneDurableToken(t *testing.T) {
	stateDir, local, fleet, _ := firstCoordinatorFleet(t)
	fleet.Members = append(fleet.Members, local)
	store, err := update.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Start(local.ID, fleet, updateTestManifest())
	if err != nil {
		t.Fatal(err)
	}
	environment := updateEnvironment{stateDir: stateDir, local: local, coordinator: local}
	run, request, err := approveFirstCoordinator(store, environment, run)
	if err != nil {
		t.Fatal(err)
	}
	status := updateinstall.Status{Schema: 1, Phase: updateinstall.RolledBack, Settings: updateinstall.Settings{StateDir: stateDir}, Request: updateinstall.Request{ID: request.ID, TargetID: request.TargetID, Generation: request.Generation, Manifest: request.Manifest}}
	writeApprovalJournal(t, status)
	if _, err := recordCoordinatorReceipt(store, run, status); err != nil {
		t.Fatal(err)
	}
	caller := updateCallFunc(func(_ context.Context, _ update.Host, _ string, _, _ any) error {
		return &update.RemoteError{Problem: `daemon: unknown control "update.control"`}
	})
	bootstrap := func(_ context.Context, retry updatebootstrap.Request, _ updatebootstrap.Config) (updateinstall.Status, error) {
		saved, err := store.Read(run.ID)
		if err != nil {
			return updateinstall.Status{}, err
		}
		index := coordinatorTargetIndex(saved, local.ID)
		if retry.ID != run.ID || retry.Generation != request.Generation || retry.Manifest.Digest() != run.ReleaseDigest || retry.RetryToken != 1 || saved.Targets[index].BootstrapRetryToken != 1 || !saved.Targets[index].Grant {
			t.Fatalf("retry changed approval or used nondurable token: %+v, %+v", retry, saved)
		}
		return updateinstall.Status{}, context.Canceled
	}
	stdout, _, err := executeCommand(t, Dependencies{UpdateCaller: caller, UpdateBootstrap: bootstrap}, "update", "retry", run.ID, "--json")
	if code, ok := StatusCode(err); !ok || code != 2 {
		t.Fatalf("interrupted retry lost pending intent: %v", err)
	}
	if !strings.Contains(stdout, run.ID) {
		t.Fatal("retry lost original run")
	}
	resumed, pending := pendingCoordinatorRetry(status)
	if !pending || resumed.RetryToken != 1 {
		t.Fatalf("helper lost retry before target journal recorded token: %+v, %v", resumed, pending)
	}
	status.RetryToken = 1
	if _, pending := pendingCoordinatorRetry(status); pending {
		t.Fatal("helper replayed already-consumed retry")
	}
	saved, err := readFirstCoordinatorRun(environment, run.ID)
	if err != nil || saved.Stopped {
		t.Fatalf("prior rollback receipt stopped the newly approved retry: %+v, %v", saved, err)
	}
	_, _, err = executeCommand(t, Dependencies{UpdateCaller: caller, UpdateBootstrap: bootstrap}, "update", "retry", run.ID, "--json")
	if code, ok := StatusCode(err); !ok || code != 2 {
		t.Fatalf("lost dispatch acknowledgment did not reuse retry token: %v", err)
	}
}

func TestFirstCoordinatorCancelSurvivesUnavailableOldDaemon(t *testing.T) {
	stateDir, local, _, _ := firstCoordinatorFleet(t)
	store, err := update.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Start(local.ID, scopedUpdateFleet("local", []update.Host{local}), updateTestManifest())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = approveFirstCoordinator(store, updateEnvironment{stateDir: stateDir, local: local}, run)
	if err != nil {
		t.Fatal(err)
	}
	caller := updateCallFunc(func(_ context.Context, _ update.Host, _ string, _, _ any) error {
		return &update.RemoteError{Problem: `daemon: unknown control "update.control"`}
	})
	_, _, _ = executeCommand(t, Dependencies{UpdateCaller: caller}, "update", "cancel", run.ID, "--json")
	saved, err := store.Read(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Cancel {
		t.Fatal("old coordinator discarded cancellation")
	}
	_, _, err = executeCommand(t, Dependencies{UpdateCaller: caller}, "update", "retry", run.ID, "--json")
	if err == nil {
		t.Fatal("cancelled scope was automatically reapproved")
	}
}

func TestFirstCoordinatorHelperRecoversBeforeInstallationJournalExists(t *testing.T) {
	stateDir, local, _, _ := firstCoordinatorFleet(t)
	store, err := update.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Start(local.ID, scopedUpdateFleet("local", []update.Host{local}), updateTestManifest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pendingFirstCoordinatorRequest(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary run authorized bootstrap: %v", err)
	}
	_, approved, err := approveFirstCoordinator(store, updateEnvironment{stateDir: stateDir, local: local}, run)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := pendingFirstCoordinatorRequest(stateDir)
	if err != nil || resumed.ID != approved.ID || resumed.Generation != approved.Generation || resumed.Manifest.Digest() != approved.Manifest.Digest() {
		t.Fatalf("helper lost approval before Accepted: %+v, %v", resumed, err)
	}
	if _, err := store.Cancel(run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pendingFirstCoordinatorRequest(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled run authorized missing-journal bootstrap: %v", err)
	}
}
