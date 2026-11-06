package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/updateinstall"
)

func TestStartSettlesVisiblePublicationBeforeAcknowledging(t *testing.T) {
	for index := range 4 {
		t.Run(fmt.Sprint(index), func(t *testing.T) { testStartPublicationBarrier(t, index) })
	}
}

func testStartPublicationBarrier(t *testing.T, index int) {
	state := filepath.Join(t.TempDir(), "state")
	store, err := OpenStore(state)
	if err != nil {
		t.Fatal(err)
	}
	fleet, manifest := testFleet(t, 1), testManifest()
	syncReal := syncOperationDirectory
	t.Cleanup(func() { syncOperationDirectory = syncReal })
	fault := errors.New("injected run directory barrier failure")
	calls := 0
	directories := []string{store.directory, filepath.Dir(store.directory), state, filepath.Dir(state)}
	syncOperationDirectory = func(path string) error {
		if path != directories[index] {
			return syncReal(path)
		}
		calls++
		return fault
	}
	first, err := store.Start(fleet.Members[0].ID, fleet, manifest)
	if !errors.Is(err, fault) {
		t.Fatalf("initial publication = %v", err)
	}
	original, err := os.ReadFile(store.path(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(original, []byte(first.ID)) {
		t.Fatal("rename did not publish the run")
	}
	for range 2 {
		reopened, openErr := OpenStore(state)
		if openErr != nil {
			t.Fatal(openErr)
		}
		before := calls
		_, err = reopened.Start(fleet.Members[0].ID, fleet, manifest)
		if !errors.Is(err, fault) || calls == before {
			t.Fatalf("retry acknowledged unconfirmed publication: barriers=%d error=%v", calls-before, err)
		}
	}
	syncOperationDirectory = syncReal
	recovered, err := store.Start(fleet.Members[0].ID, fleet, manifest)
	if err != nil || recovered.ID != first.ID {
		t.Fatalf("settlement = %s %v", recovered.ID, err)
	}
	after, err := os.ReadFile(store.path(first.ID))
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("settlement changed approval or progress: %v", err)
	}
	runs, err := store.List()
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %d %v", len(runs), err)
	}
}

func TestCoordinatorWaitsForVisibleGrantPublication(t *testing.T) {
	c, remote, run := testCoordinator(t, 1)
	host := run.Targets[0].Host
	run, err := c.Store.Change(run.ID, func(r *Run) error {
		r.Cached = true
		r.Targets[0].State, r.Targets[0].Generation = Staged, 1
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	info := remote.info[host.ID]
	info.Installation = &updateinstall.Status{Schema: 1, Phase: updateinstall.Staged, Request: updateinstall.Request{ID: run.ID, TargetID: host.ID, Generation: 1, Manifest: run.Release}}
	remote.info[host.ID] = info
	syncReal := syncOperationDirectory
	t.Cleanup(func() { syncOperationDirectory = syncReal })
	fault := errors.New("injected grant-intent barrier failure")
	syncOperationDirectory = func(path string) error {
		data, readErr := os.ReadFile(c.Store.path(run.ID))
		if readErr != nil {
			return fmt.Errorf("read grant intent at barrier: %w", readErr)
		}
		if bytes.Contains(data, []byte(`"grant": true`)) {
			return fault
		}
		return syncReal(path)
	}
	if err := c.grant(context.Background(), run.ID, 0); !errors.Is(err, fault) {
		t.Fatalf("grant-intent publication = %v", err)
	}
	if err := c.Step(context.Background()); !errors.Is(err, fault) {
		t.Fatalf("coordinator used unconfirmed grant intent: %v", err)
	}
	if len(remote.grants) != 0 || remote.stages != 0 {
		t.Fatalf("unconfirmed intent dispatched: grants=%v stages=%d", remote.grants, remote.stages)
	}
	syncOperationDirectory = syncReal
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(remote.grants) != 1 || remote.grants[0] != host.ID {
		t.Fatalf("settled intent grants = %v", remote.grants)
	}
}

func TestCancellationPublicationSurvivesBarrierFailure(t *testing.T) {
	c, _, run := testCoordinator(t, 1)
	syncReal := syncOperationDirectory
	t.Cleanup(func() { syncOperationDirectory = syncReal })
	fault := errors.New("injected cancellation barrier failure")
	syncOperationDirectory = func(path string) error {
		data, err := os.ReadFile(c.Store.path(run.ID))
		if err != nil {
			return fmt.Errorf("read cancellation at barrier: %w", err)
		}
		if bytes.Contains(data, []byte(`"cancel": true`)) {
			return fault
		}
		return syncReal(path)
	}
	cancelled, err := c.Store.Cancel(run.ID)
	if !errors.Is(err, fault) || !cancelled.Cancel {
		t.Fatalf("cancellation publication = %+v %v", cancelled, err)
	}
	if _, err := c.Store.Cancel(run.ID); !errors.Is(err, fault) {
		t.Fatalf("cancellation retry = %v", err)
	}
	syncOperationDirectory = syncReal
	cancelled, err = c.Store.Read(run.ID)
	if err != nil || !cancelled.Cancel || !cancelled.Stopped || cancelled.approvalIdentity() != run.approvalIdentity() {
		t.Fatalf("settled cancellation = %+v %v", cancelled, err)
	}
	if _, err = c.Store.Retry(run.ID); err == nil {
		t.Fatal("barrier failure restored cancelled approval authority")
	}
	approved, err := c.Store.Start(run.Coordinator, run.Fleet, run.Release)
	if err != nil || approved.ID == run.ID {
		t.Fatalf("fresh manual approval = %s %v", approved.ID, err)
	}
}
