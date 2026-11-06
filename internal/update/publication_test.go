package update

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStartSettlesVisiblePublicationBeforeAcknowledging(t *testing.T) {
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
	syncOperationDirectory = func(path string) error { calls++; return fault }
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
