package machinename

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaimCacheChangedWriterDoesNotSettleSupersededRecord(t *testing.T) {
	directory := t.TempDir()
	claim := cacheFixtureClaim(t, "destination", 1)
	if _, err := RememberClaim(t.Context(), directory, claim.ID, claim); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var barriers atomic.Int32
	syncRoot := func(root *os.Root) error {
		if filepath.Base(root.Name()) != cacheDirectory {
			return syncCacheRoot(root)
		}
		barriers.Add(1)
		current, err := readCachedClaim(root, claim.ID, func(*os.Root) error { return nil })
		if err != nil {
			return err
		}
		enteredOnce.Do(func() { close(entered) })
		// A superseded record's stalled barrier must not hold the next publisher's lock.
		if current.Revision == 1 {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return syncCacheRoot(root)
	}
	first := make(chan error, 1)
	next := claim
	next.Revision = 2
	go func() {
		_, err := rememberClaimWithSync(ctx, directory, claim.ID, next, syncRoot)
		first <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first writer never reached its cache-root barrier")
	}
	latest := claim
	latest.Revision = 3
	start := time.Now()
	_, err := RememberClaim(ctx, directory, claim.ID, latest)
	elapsed := time.Since(start)
	releaseOnce.Do(func() { close(release) })
	if firstErr := <-first; firstErr != nil {
		t.Fatalf("first publisher: %v", firstErr)
	}
	t.Logf("changed-writer cache-root barriers=%d competing-writer elapsed=%s error=%v", barriers.Load(), elapsed, err)
	if err != nil {
		t.Fatalf("highest revision lost to superseded-record barrier: %v", err)
	}
	got, readErr := CachedClaim(directory, claim.ID)
	if readErr != nil || got != latest {
		t.Fatalf("highest committed claim: %+v %v", got, readErr)
	}
	if barriers.Load() != 1 {
		t.Fatalf("changed writer requires one final publication barrier, got %d", barriers.Load())
	}
}

func TestClaimCacheWriterSettlesVisibleFailedPublication(t *testing.T) {
	for _, revision := range []uint64{7, 8} {
		t.Run(fmt.Sprintf("revision-%d", revision), func(t *testing.T) {
			directory := t.TempDir()
			claim := cacheFixtureClaim(t, "destination", 7)
			failure := errors.New("fixture post-rename sync failure")
			var observed []uint64
			failPublished := func(root *os.Root) error {
				if filepath.Base(root.Name()) != cacheDirectory {
					return syncCacheRoot(root)
				}
				got, err := readCachedClaim(root, claim.ID, func(*os.Root) error { return nil })
				if err != nil {
					return err
				}
				observed = append(observed, got.Revision)
				return failure
			}
			if changed, err := rememberClaimWithSync(t.Context(), directory, claim.ID, claim, failPublished); changed || !errors.Is(err, failure) {
				t.Fatalf("uncertain first publication acknowledged: %t %v", changed, err)
			}
			observed = nil
			next := claim
			next.Revision = revision
			if changed, err := rememberClaimWithSync(t.Context(), directory, claim.ID, next, failPublished); changed || !errors.Is(err, failure) {
				t.Fatalf("uncertain retry publication acknowledged: %t %v", changed, err)
			}
			if len(observed) != 1 || observed[0] != revision {
				t.Fatalf("writer barrier did not settle the final record: %v", observed)
			}
			if got, err := cachedClaimWithSync(directory, claim.ID, failPublished); got != (Claim{}) || !errors.Is(err, failure) {
				t.Fatalf("reader acknowledged uncertain retry: %+v %v", got, err)
			}
			var settled int
			settle := func(root *os.Root) error {
				if filepath.Base(root.Name()) == cacheDirectory {
					settled++
				}
				return syncCacheRoot(root)
			}
			if changed, err := rememberClaimWithSync(t.Context(), directory, claim.ID, next, settle); changed || err != nil || settled != 1 {
				t.Fatalf("unchanged retry failed to settle publication: %t %v barriers=%d", changed, err, settled)
			}
			if got, err := CachedClaim(directory, claim.ID); got != next || err != nil {
				t.Fatalf("recovered publication: %+v %v", got, err)
			}
		})
	}
}
