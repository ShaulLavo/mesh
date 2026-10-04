package machinename

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestClaimCacheDirectoryCreationBarriers(t *testing.T) {
	for _, component := range []string{"selected", "configuration", cacheDirectory} {
		t.Run(component, func(t *testing.T) {
			selected := filepath.Join(t.TempDir(), "selected")
			directory := filepath.Join(selected, "configuration")
			claim := cacheFixtureClaim(t, "destination", 7)
			failure := errors.New("fixture parent directory sync failure")
			var failedParents []string
			syncRoot := func(root *os.Root) error {
				if _, err := root.Lstat(component); err == nil {
					failedParents = append(failedParents, root.Name())
					return failure
				}
				return syncCacheRoot(root)
			}
			if changed, err := rememberClaimWithSync(t.Context(), directory, claim.ID, claim, syncRoot); changed || !errors.Is(err, failure) {
				t.Fatalf("creation acknowledged without parent sync: %t %v", changed, err)
			}
			if _, err := os.Stat(filepath.Join(directory, cacheDirectory, claim.ID+".json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed creation published a claim: %v", err)
			}
			if component != cacheDirectory {
				if changed, err := rememberClaimWithSync(t.Context(), directory, claim.ID, claim, syncRoot); changed || !errors.Is(err, failure) {
					t.Fatalf("recovery skipped the original failed parent barrier: %t %v", changed, err)
				}
				if len(failedParents) != 2 || failedParents[0] != failedParents[1] {
					t.Fatalf("recovery did not revisit the same failed naming parent: %v", failedParents)
				}
				t.Logf("creation and recovery both refused at parent %s", failedParents[0])
			}
			if changed, err := RememberClaim(t.Context(), directory, claim.ID, claim); !changed || err != nil {
				t.Fatalf("creation recovery: %t %v", changed, err)
			}
			if got, err := CachedClaim(directory, claim.ID); err != nil || got != claim {
				t.Fatalf("recovered claim: %+v %v", got, err)
			}
		})
	}
}

func TestClaimCacheReaderSettlesVisibleFailedPublication(t *testing.T) {
	directory := t.TempDir()
	claim := cacheFixtureClaim(t, "destination", 7)
	failure := errors.New("fixture post-rename directory sync failure")
	failPublished := func(root *os.Root) error {
		if filepath.Base(root.Name()) == cacheDirectory {
			if _, err := root.Lstat(claim.ID + ".json"); err == nil {
				return failure
			}
		}
		return syncCacheRoot(root)
	}
	if changed, err := rememberClaimWithSync(t.Context(), directory, claim.ID, claim, failPublished); changed || !errors.Is(err, failure) {
		t.Fatalf("uncertain publication acknowledged: %t %v", changed, err)
	}
	if got, err := cachedClaimWithSync(directory, claim.ID, failPublished); !errors.Is(err, failure) || got != (Claim{}) {
		t.Fatalf("reader exposed unsynced revision: %+v %v", got, err)
	}
	if got, err := CachedClaim(directory, claim.ID); err != nil || got != claim {
		t.Fatalf("durability recovery failed: %+v %v", got, err)
	}
	older := claim
	older.Revision--
	if _, err := RememberClaim(t.Context(), directory, claim.ID, older); !errors.Is(err, ErrReplay) {
		t.Fatalf("recovered revision accepted replay: %v", err)
	}
}

func TestClaimCacheReaderRejectsUnsafeDirectoryPermissions(t *testing.T) {
	directory := t.TempDir()
	claim := cacheFixtureClaim(t, "destination", 1)
	if _, err := RememberClaim(t.Context(), directory, claim.ID, claim); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // negative fixture must reject world-writable cache directories
	if err := os.Chmod(filepath.Join(directory, cacheDirectory), 0777); err != nil {
		t.Fatal(err)
	}
	if got, err := CachedClaim(directory, claim.ID); err == nil || got != (Claim{}) {
		t.Fatalf("reader exposed claim from unsafe directory: %+v %v", got, err)
	}
}

func TestClaimProjectionExtendsOnlySameNameCollisions(t *testing.T) {
	claims := []Claim{{ID: "aaaaaaaaa12345678", MachineName: "same", Revision: 1}, {ID: "baaaaaaaa12345678", MachineName: "same", Revision: 1}, {ID: "aaaaaaaaa12345678", MachineName: "other", Revision: 1}}
	want := Project(claims)
	if want[0].Suffix != "12345678" || len(want[1].Suffix) != len(claims[0].ID) || want[1].Suffix == want[2].Suffix {
		t.Fatalf("wrong suffix fallback: %+v", want)
	}
	for _, order := range [][]Claim{{claims[2], claims[1], claims[0]}, {claims[1], claims[0], claims[2]}} {
		if got := Project(order); !reflect.DeepEqual(got, want) {
			t.Fatalf("order-dependent projection: %+v", got)
		}
	}
}
