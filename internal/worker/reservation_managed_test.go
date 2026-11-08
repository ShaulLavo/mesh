package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
)

func TestManagedReservationUsesStrictPublicationAndConfirmation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "s")
	ref, err := Reserve(root, session.Managed)
	if err != nil {
		t.Fatal(err)
	}
	if ref.BootID == "" {
		t.Fatal("managed allocation has unknown boot")
	}
	files, err := execution.OpenFiles(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	for _, name := range []string{".reservation", ".launching"} {
		var saved session.Reservation
		relative := filepath.Join("s", ref.ID, name)
		if err := files.Read(relative, &saved); err != nil {
			t.Fatal(err)
		}
		if saved != ref {
			t.Fatalf("%s reference changed", name)
		}
		if err := files.Confirm(relative); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateReservation(root, ref); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ref.ID)
	if err := os.Remove(filepath.Join(dir, ".launching")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, ".reservation"), filepath.Join(dir, ".launching")); err != nil {
		t.Fatal(err)
	}
	if err := validateReservation(root, ref); err == nil {
		t.Fatal("managed launch followed a non-authoritative marker")
	}
}

func TestManagedReservationRejectsReusedInstanceAndBoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "s")
	ref, err := Reserve(root, session.Managed)
	if err != nil {
		t.Fatal(err)
	}
	wrong := ref
	wrong.Instance = "11111111111111111111111111111111"
	if err := validateReservation(root, wrong); err == nil {
		t.Fatal("accepted another instance")
	}
	wrong = ref
	wrong.BootID = "another-boot"
	data, err := json.Marshal(wrong)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".reservation", ".launching"} {
		if err := os.WriteFile(filepath.Join(root, ref.ID, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateReservation(root, wrong); err == nil {
		t.Fatal("accepted another boot")
	}
	if _, err := RenewRecovery(root, ref); err == nil {
		t.Fatal("renewed managed ownership")
	}
}
