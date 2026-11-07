package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/session"
)

func runtimeTestRecord(t *testing.T) ServiceRuntime {
	t.Helper()
	recipe, err := meshserve.Normalize(meshserve.Service{Name: "dev", Kind: meshserve.Proxy, Target: "5173", Demand: &meshserve.Demand{Command: "run", Cwd: "/tmp", Env: []string{"MODE=one"}, ReadyTimeout: time.Second, KeepRunning: true}})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := session.NewReservation("ABCD", "boot", session.Managed)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	return ServiceRuntime{Name: "dev", Generation: "1234567890abcdef1234567890abcdef", Version: 1, Phase: ServiceReadiness, Attempt: &ServiceAttempt{Reference: ref, Launched: recipe, StartedAt: now, Deadline: now.Add(time.Second)}}
}

func TestServiceRuntimeRoundTripAndCAS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.db")
	store, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	record := runtimeTestRecord(t)
	if err := store.CompareServiceRuntime(t.Context(), nil, &record); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	got, err := store.GetServiceRuntime(t.Context(), record.Name)
	if err != nil || !got.Equal(record) {
		t.Fatalf("roundtrip = %+v, %v", got, err)
	}
	// A retry of an accepted write still performs its synchronous barrier.
	if err := store.CompareServiceRuntime(t.Context(), nil, &record); err != nil {
		t.Fatal(err)
	}
	next := got
	next.Version++
	next.Failures = 3
	if err := store.CompareServiceRuntime(t.Context(), &got, &next); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareServiceRuntime(t.Context(), &got, nil); !errors.Is(err, ErrServiceRuntimeConflict) {
		t.Fatalf("stale delete = %v", err)
	}
	next.Attempt, next.Phase = nil, ServiceNone
	next.Version++
	before, _ := store.GetServiceRuntime(t.Context(), record.Name)
	if err := store.CompareServiceRuntime(t.Context(), &before, &next); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertService(t.Context(), record.Attempt.Launched); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareServiceRuntime(t.Context(), &next, nil); !errors.Is(err, ErrServiceRuntimeConflict) {
		t.Fatalf("bound definition delete = %v", err)
	}
	if err := store.DeleteService(t.Context(), record.Name); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareServiceRuntime(t.Context(), &next, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.CompareServiceRuntime(t.Context(), &next, nil); err != nil {
		t.Fatal(err)
	}
}

func TestServiceRuntimeRejectsIncompleteOwnership(t *testing.T) {
	for _, phase := range []ServicePhase{ServiceStarting, ServiceReadiness, ServiceReady, ServiceCleanup} {
		t.Run(string(phase), func(t *testing.T) {
			record := runtimeTestRecord(t)
			record.Phase, record.Attempt = phase, nil
			if err := validateServiceRuntime(record); err == nil {
				t.Fatal("phase without ownership accepted")
			}
		})
	}
}
