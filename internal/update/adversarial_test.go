package update

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/updateinstall"
)

func TestTrustIfEmptyNeverOverridesAnExplicitPolicy(t *testing.T) {
	id, _ := testIdentity(t)
	for _, policy := range []Policy{{Version: 1}, {Version: 2, Keys: []string{id}}, {Version: 0}} {
		directory := t.TempDir()
		path := filepath.Join(directory, "updates", "administrators.json")
		if err := writeJSON(path, policy); err != nil {
			t.Fatal(err)
		}
		if err := Trust(directory, id, true); err == nil {
			t.Fatalf("bootstrap replaced explicit policy %+v", policy)
		}
		var after Policy
		if err := readJSON(path, &after); err != nil || after.Version != policy.Version || len(after.Keys) != len(policy.Keys) {
			t.Fatalf("rejected enrollment changed policy: %+v, %v", after, err)
		}
	}
}

func TestSavedTargetsCannotDriftFromApprovedFleet(t *testing.T) {
	coordinator, _, run := testCoordinator(t, 2)
	_, err := coordinator.Store.Change(run.ID, func(current *Run) error { current.Targets[0].Host.Endpoint = "ws://other.invalid/mesh"; return nil })
	if err == nil {
		t.Fatal("mutable target redirected approved installation")
	}
	run.Targets[0].Host.Endpoint = "ws://other.invalid/mesh"
	if err := writeJSON(coordinator.Store.path(run.ID), run); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Store.Read(run.ID); err == nil {
		t.Fatal("corrupt target membership accepted from disk")
	}
}

func TestExplicitOfflineDependencyBlocksRouterActivation(t *testing.T) {
	fleet := testFleet(t, 2)
	fleet.Members[0].DependsOn = []string{fleet.Members[1].ID}
	run := Run{Cached: true, Coordinator: fleet.Members[1].ID, Targets: []Target{{Host: fleet.Members[0], State: Offline}, {Host: fleet.Members[1], State: Staged}}}
	if canGrant(run, 1) {
		t.Fatal("offline explicit dependency was ignored")
	}
	run.Targets[0].Host.DependsOn = nil
	if !canGrant(run, 1) {
		t.Fatal("coordinator-last alone blocked on an offline host")
	}
}

func TestNewerIncompatibleBuildRequiresIntervention(t *testing.T) {
	coordinator, remote, run := testCoordinator(t, 1)
	id := run.Targets[0].Host.ID
	info := remote.info[id]
	info.Health.Build.Version, info.Health.Build.StateVersion = "v9.0.0", 99
	remote.info[id] = info
	if err := coordinator.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Store.Read(run.ID)
	if err != nil || result.Targets[0].State != Failed || result.ExitCode() != 1 || len(remote.grants) != 0 {
		t.Fatalf("incompatible newer host reported success: %+v, %v", result, err)
	}
}

type cancelRaceCaller struct{}

func (cancelRaceCaller) Call(context.Context, Host, string, any, any) error {
	return &RemoteError{Problem: updateinstall.ErrAlreadyGranted.Error()}
}

func TestCancelRaceKeepsIssuedGrantVisible(t *testing.T) {
	coordinator, _, run := testCoordinator(t, 1)
	coordinator.Remote = cancelRaceCaller{}
	if err := coordinator.cancelTarget(context.Background(), run, 0); err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Store.Read(run.ID)
	if err != nil || !result.Targets[0].Grant || result.Targets[0].State != Granted || result.Done() {
		t.Fatalf("cancel race hid authorized activation: %+v, %v", result, err)
	}
}

func TestIdenticalInstallationJoinsWithoutIssuingAnotherGrant(t *testing.T) {
	coordinator, remote, run := testCoordinator(t, 2)
	id := run.Targets[0].Host.ID
	owner := strings.Repeat("f", 32)
	info := remote.info[id]
	info.Installation = &updateinstall.Status{Schema: 1, Phase: updateinstall.Staged, Request: updateinstall.Request{ID: owner, TargetID: id, Generation: 9, Manifest: run.Release, Current: info.Health.Build}}
	remote.info[id] = info
	if err := coordinator.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined, err := coordinator.Store.Read(run.ID)
	if err != nil || joined.Targets[0].InstallationID != owner || joined.Targets[0].Generation != 9 || len(remote.grants) != 0 {
		t.Fatalf("duplicate release was not joined: %+v, %v", joined, err)
	}
	if remote.info[id].Installation.Phase != updateinstall.Staged {
		t.Fatal("joining operation granted another coordinator's installation")
	}
	info.Installation.Phase = updateinstall.Granted
	remote.info[id] = info
	if err := coordinator.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined, _ = coordinator.Store.Read(run.ID)
	if !joined.Targets[0].Grant {
		t.Fatal("joined issued grant was not accounted for")
	}
	if _, err := coordinator.Store.Cancel(run.ID); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if remote.info[id].Installation.Phase != updateinstall.Granted {
		t.Fatal("joining cancellation revoked another operation's grant")
	}
	remote.complete(id)
	if err := coordinator.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	final, err := coordinator.Store.Read(run.ID)
	if err != nil || final.Targets[0].State != Updated || final.Targets[0].Grant {
		t.Fatalf("joined receipt was not verified: %+v, %v", final, err)
	}
}

func TestCancellationRequiresMatchingTargetReceipt(t *testing.T) {
	coordinator, remote, run := testCoordinator(t, 1)
	if err := coordinator.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, _ := coordinator.Store.Read(run.ID)
	info := remote.info[current.Targets[0].Host.ID]
	info.Installation.Phase = updateinstall.Staged
	info.Installation.Request.TargetID = "another-target"
	remote.info[current.Targets[0].Host.ID] = info
	if err := coordinator.cancelTarget(context.Background(), current, 0); err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Store.Read(run.ID)
	if err != nil || result.Targets[0].State != Failed {
		t.Fatalf("mismatched cancellation was acknowledged: %+v, %v", result, err)
	}
}

func TestCoordinatorBootstrapMustCommitBeforeAnyFurtherGrant(t *testing.T) {
	fleet := testFleet(t, 3)
	run := Run{Cached: true, CoordinatorBootstrap: true, Coordinator: fleet.Members[2].ID, Targets: []Target{
		{Host: fleet.Members[0], State: Updated},
		{Host: fleet.Members[1], State: Staged},
		{Host: fleet.Members[2], State: Granted, Grant: true},
	}}
	if canGrant(run, 1) {
		t.Fatal("already-current remote bypassed coordinator bootstrap verification")
	}
	run.Targets[2].State, run.Targets[2].Grant = Updated, false
	if !canGrant(run, 1) {
		t.Fatal("verified coordinator did not release fleet activation")
	}
}
