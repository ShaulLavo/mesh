package machinename

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
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
	other := cacheFixtureClaim(t, "other", 1)
	if _, err := RememberClaim(t.Context(), directory, other.ID, other); err != nil {
		t.Fatal(err)
	}
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
	owners := []string{other.ID, claim.ID}
	if claims, err := cachedClaimsWithSync(directory, owners, failPublished); claims != nil || !errors.Is(err, failure) {
		t.Fatalf("batch exposed uncertain publication: %+v %v", claims, err)
	}
	claims, err := CachedClaims(directory, owners)
	if err != nil || claims[claim.ID] != claim || claims[other.ID] != other {
		t.Fatalf("batch failed to settle visible publication: %+v %v", claims, err)
	}
}

func TestClaimCacheBatchMissingOwners(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "missing")
	claim := cacheFixtureClaim(t, "destination", 1)
	if claims, err := CachedClaims(directory, []string{claim.ID}); err != nil || len(claims) != 0 {
		t.Fatalf("missing cache: %+v %v", claims, err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache read created configuration directory: %v", err)
	}
	if _, err := RememberClaim(t.Context(), directory, claim.ID, claim); err != nil {
		t.Fatal(err)
	}
	other := cacheFixtureClaim(t, "other", 1)
	claims, err := CachedClaims(directory, []string{claim.ID, other.ID, claim.ID})
	if err != nil || len(claims) != 1 || claims[claim.ID] != claim {
		t.Fatalf("missing/duplicate owners: %+v %v", claims, err)
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

func TestClaimCacheBatchConcurrentRevisionsStayFresh(t *testing.T) {
	directory := t.TempDir()
	initial := []Claim{cacheFixtureClaim(t, "first", 1), cacheFixtureClaim(t, "second", 1)}
	owners := []string{initial[0].ID, initial[1].ID}
	for _, claim := range initial {
		if _, err := RememberClaim(t.Context(), directory, claim.ID, claim); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	var writers sync.WaitGroup
	for _, claim := range initial {
		writers.Go(func() {
			<-start
			writeBatchFixtureRevisions(t, directory, claim)
		})
	}
	close(start)
	previous := map[string]uint64{}
	var successes, refusals int
	for range 32 {
		claims, err := CachedClaims(directory, owners)
		if err != nil && claims != nil {
			t.Fatalf("refused batch exposed partial claims: %+v, error=%v", claims, err)
		}
		if errors.Is(err, errCacheFileChanged) {
			refusals++
			continue
		}
		if err != nil {
			t.Error(err)
			break
		}
		successes++
		checkBatchFixtureRevisions(t, owners, claims, previous)
	}
	t.Logf("concurrent reads: %d successful, %d inode-change refusals", successes, refusals)
	writers.Wait()
	claims, err := CachedClaims(directory, owners)
	if err != nil || claims[owners[0]].Revision != 16 || claims[owners[1]].Revision != 16 {
		t.Fatalf("batch missed final concurrent revisions: %+v %v", claims, err)
	}
}

func checkBatchFixtureRevisions(t *testing.T, owners []string, claims map[string]Claim, previous map[string]uint64) {
	t.Helper()
	for _, owner := range owners {
		if claims[owner].ID != owner || claims[owner].Revision < previous[owner] {
			t.Errorf("batch lost owner/revision: %+v previous=%d", claims[owner], previous[owner])
		}
		previous[owner] = claims[owner].Revision
	}
}

func writeBatchFixtureRevisions(t *testing.T, directory string, claim Claim) {
	t.Helper()
	for revision := uint64(2); revision <= 16; revision++ {
		claim.Revision = revision
		if _, err := RememberClaim(t.Context(), directory, claim.ID, claim); err != nil {
			t.Error(err)
			return
		}
	}
}
