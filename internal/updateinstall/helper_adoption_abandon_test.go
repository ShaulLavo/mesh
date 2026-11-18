package updateinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func TestAbandonAdoptionHelperRemovesOnlyItsOwnInitialHelper(t *testing.T) {
	f := newFreshHelperFixture(t, promotionSystemd)
	artifact, err := f.source.Artifact(release.CurrentPlatform())
	if err != nil {
		t.Fatal(err)
	}
	operation, pin := strings.Repeat("a", 32), strings.Repeat("b", 64)
	approve := func(_ context.Context, commit func() error) error { return commit() }
	installed, err := PrepareAdoptionHelper(t.Context(), f.cfg, f.source, artifact.BinarySHA256, AdoptionHelperApproval{Operation: operation, Pin: pin, Authorize: approve})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AbandonAdoptionHelper(t.Context(), f.cfg, strings.Repeat("c", 32), pin); err == nil {
		t.Fatal("another operation abandoned this adoption's helper")
	}
	if _, err := os.Stat(f.active); err != nil {
		t.Fatalf("refused abandon stopped the helper: %v", err)
	}
	restored, err := AbandonAdoptionHelper(t.Context(), f.cfg, operation, pin)
	if err != nil || !restored {
		t.Fatalf("abandon = %v, %v", restored, err)
	}
	owned := []string{f.active, filepath.Join(f.cfg.ServiceDir, "mesh-update-helper.service"), helperRecord(f.cfg.StateDir), adoptionHelperPreparationPath(f.cfg.StateDir), installed.Executable}
	for _, path := range owned {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("abandon kept adoption-owned %s: %v", path, err)
		}
	}
	if restored, err := AbandonAdoptionHelper(t.Context(), f.cfg, operation, pin); err != nil || restored {
		t.Fatalf("repeat abandon = %v, %v", restored, err)
	}
	next := AdoptionHelperApproval{Operation: strings.Repeat("d", 32), Pin: pin, Authorize: approve}
	if _, err := PrepareAdoptionHelper(t.Context(), f.cfg, f.source, artifact.BinarySHA256, next); err != nil {
		t.Fatalf("a later adoption could not prepare its helper: %v", err)
	}
}
