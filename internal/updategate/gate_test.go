package updategate

import (
	"errors"
	"os"
	"testing"
)

func TestGatePersistsAndOnlyItsOwnerCanClearIt(t *testing.T) {
	dir := t.TempDir()
	if err := Check(dir); err != nil {
		t.Fatal(err)
	}
	if err := Set(dir, "rollout-1"); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir); !errors.Is(err, ErrUpdating) {
		t.Fatalf("gate = %v", err)
	}
	if err := Clear(dir, "other-rollout"); err == nil {
		t.Fatal("another operation cleared gate")
	}
	if err := Check(dir); !errors.Is(err, ErrUpdating) {
		t.Fatalf("gate lost: %v", err)
	}
	if err := Clear(dir, "rollout-1"); err != nil {
		t.Fatal(err)
	}
	if err := Clear(dir, "rollout-1"); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir); err != nil {
		t.Fatal(err)
	}
}

func TestUnreadableGateFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(Path(dir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir); err == nil {
		t.Fatal("invalid gate allowed launch")
	}
}
