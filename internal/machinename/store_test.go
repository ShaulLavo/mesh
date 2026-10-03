package machinename

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/shaul/mesh/internal/identity"
)

func nameFixture(t *testing.T) (*Store, string, string) {
	t.Helper()
	directory := t.TempDir()
	host, _, err := identity.LoadOrCreate(directory)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(t.Context(), directory, host.ID, "office-pc")
	if err != nil {
		t.Fatal(err)
	}
	return store, directory, host.ID
}

func TestNameStateDurableIdempotentAndRevisionBound(t *testing.T) {
	store, directory, id := nameFixture(t)
	keyBefore, err := os.ReadFile(filepath.Join(directory, "identity.key")) //nolint:gosec // test-owned identity; compare privately without printing
	if err != nil {
		t.Fatal(err)
	}
	initial := store.Current()
	if initial.Revision != 1 || initial.ID != id || initial.MachineName != "office-pc" {
		t.Fatal("initial name claim differs")
	}
	claim, changed, err := store.Rename(t.Context(), id, "  Travel-PC ", 1)
	if err != nil || !changed || claim.Revision != 2 || claim.MachineName != "travel-pc" {
		t.Fatalf("first rename changed=%v revision=%d error=%v", changed, claim.Revision, err)
	}
	restarted, err := Open(t.Context(), directory, id, "different-os-host")
	if err != nil || restarted.Current() != claim {
		t.Fatalf("restart lost authoritative claim: %v", err)
	}
	for _, expected := range []uint64{1, 2} {
		retried, changed, err := restarted.Rename(t.Context(), id, "travel-pc", expected)
		if err != nil || changed || retried != claim {
			t.Fatalf("retry advanced name revision: changed=%v error=%v", changed, err)
		}
	}
	if _, _, err := restarted.Rename(t.Context(), id, "office-pc", 1); !errors.Is(err, ErrRevision) {
		t.Fatalf("stale request accepted: %v", err)
	}
	if _, _, err := restarted.Rename(t.Context(), "another-host", "new-pc", 2); !errors.Is(err, ErrTarget) {
		t.Fatalf("wrong target accepted: %v", err)
	}
	if _, _, err := restarted.Rename(t.Context(), id, "ls", 2); err == nil {
		t.Fatal("reserved command accepted")
	}
	if _, _, err := restarted.Rename(t.Context(), id, "third-pc", 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := restarted.Rename(t.Context(), id, "travel-pc", 1); !errors.Is(err, ErrRevision) {
		t.Fatal("old committed rename replayed after another rename")
	}
	keyAfter, err := os.ReadFile(filepath.Join(directory, "identity.key")) //nolint:gosec // test-owned identity; compare privately without printing
	if err != nil || string(keyBefore) != string(keyAfter) {
		t.Fatal("rename replaced cryptographic identity")
	}
}

func TestConcurrentNamesSerializeOneObservedRevision(t *testing.T) {
	store, _, id := nameFixture(t)
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			name := "first-pc"
			if i%2 != 0 {
				name = "second-pc"
			}
			_, _, err := store.Rename(t.Context(), id, name, 1)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, ErrRevision) {
			t.Fatal(err)
		}
	}
	if store.Current().Revision != 2 {
		t.Fatal("concurrent retries incremented the revision twice")
	}
}

func TestNameStateRejectsInvalidIdentityAndFile(t *testing.T) {
	for _, mode := range []string{"symlink", "permissions", "wrong-identity", "unknown-field", "trailing", "oversize", "zero-revision", "invalid-receipt"} {
		t.Run(mode, func(t *testing.T) {
			_, directory, id := nameFixture(t)
			path := filepath.Join(directory, stateName)
			switch mode {
			case "symlink":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".original", path); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // intentionally unsafe permissions exercise rejection
					t.Fatal(err)
				}
			default:
				contents, err := os.ReadFile(path) //nolint:gosec // fixed state file under t.TempDir
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if err := json.Unmarshal(contents, &fields); err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "wrong-identity":
					fields["id"] = "another-host"
				case "unknown-field":
					fields["alias"] = "forged-pc"
				case "zero-revision":
					fields["revision"] = 0
				case "invalid-receipt":
					fields["previousRevision"] = 8
				}
				contents, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "trailing" {
					contents = append(contents, []byte(" {}")...)
				}
				if mode == "oversize" {
					contents = make([]byte, maximumStateBytes+1)
				}
				if err := os.WriteFile(path, contents, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Open(t.Context(), directory, id, "replacement-pc"); err == nil {
				t.Fatal("invalid existing name state silently replaced")
			}
		})
	}
}

func TestNameStateCancellationWriteFailureAndOverflow(t *testing.T) {
	store, directory, id := nameFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := store.Rename(ctx, id, "new-pc", 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	store.current.Revision = math.MaxUint64
	if _, _, err := store.Rename(t.Context(), id, "new-pc", math.MaxUint64); err == nil {
		t.Fatal("revision wrapped to zero")
	}
	store.current.Revision = 1
	path := filepath.Join(directory, stateName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Rename(t.Context(), id, "new-pc", 1); err == nil {
		t.Fatal("failed publication acknowledged")
	}
	if store.Current().Revision != 1 || store.Current().MachineName != "office-pc" {
		t.Fatal("failed write changed in-memory claim")
	}
}

func TestNameStateDirectorySyncFailureCannotAcknowledgeOrOverwritePendingCommit(t *testing.T) {
	store, directory, id := nameFixture(t)
	initial := store.Current()
	failed := errors.New("fixture directory sync failed")
	store.syncDirectory = func(string) error { return failed }
	if _, _, err := store.Rename(t.Context(), id, "pending-pc", 1); !errors.Is(err, failed) {
		t.Fatal("uncertain directory durability acknowledged")
	}
	if store.Current() != initial {
		t.Fatal("uncertain commit was published")
	}
	persisted, err := readRecord(filepath.Join(directory, stateName))
	if err != nil || persisted.Revision != 2 || persisted.MachineName != "pending-pc" {
		t.Fatal("fixture did not fail after atomic publication")
	}
	if _, _, err := store.Rename(t.Context(), id, "overwrite-pc", 1); !errors.Is(err, failed) {
		t.Fatal("failed sync permitted another rename")
	}
	store.syncDirectory = syncDirectory
	if _, _, err := store.Rename(t.Context(), id, "overwrite-pc", 1); !errors.Is(err, ErrRevision) || store.Current() != persisted.Claim {
		t.Fatal("stale request overwrote the recovered durable commit")
	}
	claim, _, err := store.Rename(t.Context(), id, "pending-pc", 1)
	if err != nil || claim != persisted.Claim || store.pending != nil {
		t.Fatalf("retry did not complete the original durable commit: %v", err)
	}
	restarted, err := Open(t.Context(), directory, id, "os-pc")
	if err != nil || restarted.Current() != claim {
		t.Fatalf("restart lost recovered commit: %v", err)
	}
}
