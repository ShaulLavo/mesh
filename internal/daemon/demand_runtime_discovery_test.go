package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/worker"
)

func TestRuntimeDiscoveryIncludesUnpublishedRawDirectories(t *testing.T) {
	for _, kind := range []string{"marker-with-meta", "missing-meta", "old-marker"} {
		t.Run(kind, func(t *testing.T) { testRuntimeDiscoveryRawDirectory(t, kind) })
	}
}
func testRuntimeDiscoveryRawDirectory(t *testing.T, kind string) {
	lifecycle := labelledTestLifecycle(t, &lifecycleTestCatalog{}, nil)
	ref, err := worker.Reserve(lifecycle.sessionsDir, session.Managed)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(lifecycle.sessionsDir, ref.ID)
	if kind == "marker-with-meta" {
		err = worker.WriteMeta(dir, worker.Meta{ID: ref.ID, Reservation: &ref, BootID: ref.BootID, Label: "serve /dev", Command: []string{"sh", "-lc", "run"}, Cwd: "/tmp", State: worker.StateRunning})
		if err != nil {
			t.Fatal(err)
		}
	}
	if kind == "missing-meta" {
		if err := os.Remove(paths.Launching(dir)); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "old-marker" {
		old := time.Now().Add(-24 * time.Hour)
		if err := os.Chtimes(paths.Launching(dir), old, old); err != nil {
			t.Fatal(err)
		}
	}
	candidate, found, err := lifecycle.findLabelled(t.Context(), "serve /dev")
	if kind == "marker-with-meta" {
		if err != nil || !found || candidate.ID != ref.ID {
			t.Fatalf("published marker candidate = %+v %v %v", candidate, found, err)
		}
		return
	}
	if err == nil || found {
		t.Fatalf("raw unknown reported absent: %+v %v %v", candidate, found, err)
	}
}

func TestRuntimeExactFateDoesNotInferExitFromPIDOrMissingDirectory(t *testing.T) {
	lifecycle := labelledTestLifecycle(t, &lifecycleTestCatalog{}, nil)
	ref, err := worker.Reserve(lifecycle.sessionsDir, session.Managed)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(lifecycle.sessionsDir, ref.ID)
	meta := worker.Meta{ID: ref.ID, Reservation: &ref, BootID: ref.BootID, State: worker.StateRunning, PID: 1 << 30}
	if err := worker.WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}
	if got := lifecycle.ownedFate(ref); got.fate != sessionLive {
		t.Fatalf("numeric PID became authority: %+v", got)
	}
	meta.Reservation = nil
	if err := worker.WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}
	if got := lifecycle.ownedFate(ref); got.fate != sessionUnknown {
		t.Fatalf("unknown instance = %+v", got)
	}
	if err := lifecycle.stopOwned(t.Context(), ref); err == nil {
		t.Fatal("unknown instance accepted stop")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if got := lifecycle.ownedFate(ref); got.fate != sessionUnknown {
		t.Fatalf("missing directory = %+v", got)
	}
}

func TestRuntimeNoRecordNeverGuessesLaunchedEnvironment(t *testing.T) {
	f := newRuntimeFixture(t)
	f.sessions.adoptFor, f.sessions.adoptID = f.service.Label(), "ABCD"
	f.sync(t)
	if starts, stops := f.sessions.counts(); starts != 0 || stops != 0 {
		t.Fatalf("uncertified env: starts %d stops %d", starts, stops)
	}
	if err := f.manager.Start(context.Background(), "dev"); err == nil {
		t.Fatal("uncertified launch became current")
	}
}

func TestManagedLifecyclePassesExactReservationAndEnvironment(t *testing.T) {
	lifecycle := labelledTestLifecycle(t, &lifecycleTestCatalog{}, func(cfg worker.LaunchConfig) (worker.Launched, error) {
		if cfg.Reservation == nil || cfg.Reservation.Purpose != session.Managed || cfg.Reservation.BootID == "" {
			t.Errorf("managed launch lost reference: %+v", cfg)
		}
		if len(cfg.Env) != 2 || cfg.Env[0] != "BASE=daemon" || cfg.Env[1] != "MODE=recipe" {
			t.Errorf("managed environment = %v", cfg.Env)
		}
		return worker.Launched{Meta: worker.Meta{ID: cfg.Reservation.ID, Reservation: cfg.Reservation, BootID: cfg.Reservation.BootID}}, nil
	})
	lifecycle.env = []string{"BASE=daemon"}
	ref, err := lifecycle.reserveLabelled(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	service := keptService()
	service.Demand.Env = []string{"MODE=recipe"}
	if err := lifecycle.startReserved(t.Context(), ref, service); err != nil {
		t.Fatal(err)
	}
}
