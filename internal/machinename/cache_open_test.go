package machinename

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestClaimCacheOpenedDescriptorRefusesReplacementAndKeepsStatError(t *testing.T) {
	directory := t.TempDir()
	claim := cacheFixtureClaim(t, "destination", 1)
	if _, err := RememberClaim(t.Context(), directory, claim.ID, claim); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Join(directory, cacheDirectory))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	filename := claim.ID + ".json"
	inspected, err := root.Lstat(filename)
	if err != nil {
		t.Fatal(err)
	}
	claim.Revision = 2
	if _, err := RememberClaim(t.Context(), directory, claim.ID, claim); err != nil {
		t.Fatal(err)
	}
	file, err := root.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if err := verifyOpenedCacheFile(file, inspected); !errors.Is(err, errCacheFileChanged) {
		t.Fatalf("replacement accepted after inspecting old inode: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := verifyOpenedCacheFile(file, inspected); !errors.Is(err, os.ErrClosed) || errors.Is(err, errCacheFileChanged) {
		t.Fatalf("descriptor failure misclassified as concurrent replacement: %v", err)
	}
	claims, err := CachedClaims(directory, []string{claim.ID})
	if err != nil || claims[claim.ID] != claim {
		t.Fatalf("settled replacement lost latest revision: %+v %v", claims, err)
	}
}
