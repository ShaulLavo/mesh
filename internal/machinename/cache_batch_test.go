package machinename

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestClaimCacheBatchSettlesOnePublicationBarrier(t *testing.T) {
	directory := t.TempDir()
	owners := make([]string, 3)
	for i := range owners {
		claim := cacheFixtureClaim(t, "destination", uint64(i+1))
		owners[i] = claim.ID
		if _, err := RememberClaim(t.Context(), directory, claim.ID, claim); err != nil {
			t.Fatal(err)
		}
	}
	var barriers int
	syncRoot := func(root *os.Root) error {
		if filepath.Base(root.Name()) == cacheDirectory {
			barriers++
		}
		return syncCacheRoot(root)
	}
	claims, err := cachedClaimsWithSync(directory, owners, syncRoot)
	if err != nil || len(claims) != len(owners) {
		t.Fatalf("batch claims: %+v %v", claims, err)
	}
	for i, owner := range owners {
		if claims[owner].Revision != uint64(i+1) {
			t.Fatalf("owner %s revision=%d", owner, claims[owner].Revision)
		}
	}
	if barriers != 1 {
		t.Fatalf("batch publication barriers=%d, want 1 after all records", barriers)
	}
}

func TestClaimCacheBatchFailedBarrierReturnsNoClaims(t *testing.T) {
	directory := t.TempDir()
	claim := cacheFixtureClaim(t, "destination", 7)
	failure := errors.New("fixture failed batch publication barrier")
	failPublished := func(root *os.Root) error {
		if filepath.Base(root.Name()) == cacheDirectory {
			return failure
		}
		return syncCacheRoot(root)
	}
	if changed, err := rememberClaimWithSync(t.Context(), directory, claim.ID, claim, failPublished); changed || !errors.Is(err, failure) {
		t.Fatalf("uncertain publication acknowledged: %t %v", changed, err)
	}
	if claims, err := cachedClaimsWithSync(directory, []string{claim.ID}, failPublished); claims != nil || !errors.Is(err, failure) {
		t.Fatalf("batch exposed uncertain publication: %+v %v", claims, err)
	}
	claims, err := CachedClaims(directory, []string{claim.ID})
	if err != nil || claims[claim.ID] != claim {
		t.Fatalf("batch failed to settle visible publication: %+v %v", claims, err)
	}
}

func TestClaimCacheBatchFailureReturnsNoPartialClaims(t *testing.T) {
	directory := t.TempDir()
	claim := cacheFixtureClaim(t, "destination", 1)
	if _, err := RememberClaim(t.Context(), directory, claim.ID, claim); err != nil {
		t.Fatal(err)
	}
	if claims, err := CachedClaims(directory, []string{claim.ID, "invalid"}); claims != nil || err == nil {
		t.Fatalf("invalid owner returned partial claims: %+v %v", claims, err)
	}
	other := cacheFixtureClaim(t, "other", 1)
	if err := os.WriteFile(filepath.Join(directory, cacheDirectory, other.ID+".json"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if claims, err := CachedClaims(directory, []string{claim.ID, other.ID}); claims != nil || err == nil {
		t.Fatalf("corrupt record returned partial claims: %+v %v", claims, err)
	}
}
