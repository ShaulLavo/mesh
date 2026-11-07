package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
)

func TestReservationOriginProtectsOnlyExecutionAndUnknown(t *testing.T) {
	for _, purpose := range []session.Purpose{session.Generic, session.Managed, session.Recovery, session.Execution} {
		t.Run(string(purpose), func(t *testing.T) { checkReservationOrigin(t, purpose) })
	}
}

func checkReservationOrigin(t *testing.T, purpose session.Purpose) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "s")
	ref, err := Reserve(root, purpose)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ref.ID)
	if err := os.Remove(filepath.Join(dir, ".launching")); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeta(dir, Meta{ID: ref.ID, Reservation: &ref}); err != nil {
		t.Fatal(err)
	}
	want := purpose == session.Execution
	if got := execution.ProtectedSession(root, ref.ID); got != want {
		t.Fatalf("published %s protection = %t, want %t", purpose, got, want)
	}
	if err := os.Remove(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatal(err)
	}
	if got := execution.ProtectedSession(root, ref.ID); got != want {
		t.Fatalf("static %s origin without metadata = %t, want %t", purpose, got, want)
	}
	if err := os.Remove(filepath.Join(dir, ".reservation")); err != nil {
		t.Fatal(err)
	}
	if !execution.ProtectedSession(root, ref.ID) {
		t.Fatal("lost reservation and metadata became permission to delete")
	}
}
