package machinename

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
)

func cacheFixtureClaim(t *testing.T, name string, revision uint64) Claim {
	t.Helper()
	host, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Claim{ID: host.ID, MachineName: name, Revision: revision}
}

func TestClaimCacheRevisionReplayEquivocationAndRestart(t *testing.T) {
	dir := t.TempDir()
	claim := cacheFixtureClaim(t, "destination", 7)
	changed, err := RememberClaim(context.Background(), dir, claim.ID, claim)
	if err != nil || !changed {
		t.Fatalf("first claim: changed=%t err=%v", changed, err)
	}
	changed, err = RememberClaim(context.Background(), dir, claim.ID, claim)
	if err != nil || changed {
		t.Fatalf("duplicate claim: changed=%t err=%v", changed, err)
	}
	for _, test := range []struct {
		name     string
		revision uint64
		want     error
	}{
		{"destination", 6, ErrReplay},
		{"another-name", 7, ErrEquivocation},
	} {
		next := claim
		next.MachineName, next.Revision = test.name, test.revision
		if _, err := RememberClaim(context.Background(), dir, claim.ID, next); !errors.Is(err, test.want) {
			t.Fatalf("rejected claim: got %v want %v", err, test.want)
		}
	}
	claim.MachineName, claim.Revision = "renamed", math.MaxUint64
	if _, err := RememberClaim(context.Background(), dir, claim.ID, claim); err != nil {
		t.Fatal(err)
	}
	got, err := CachedClaim(dir, claim.ID)
	if err != nil || got != claim {
		t.Fatalf("reopened cache: %+v err=%v", got, err)
	}
	info, err := os.Stat(filepath.Join(dir, cacheDirectory, claim.ID+".json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache permissions: info=%v err=%v", info, err)
	}
}

func TestClaimCacheRefusesForeignMalformedAndCancelledInput(t *testing.T) {
	dir := t.TempDir()
	claim := cacheFixtureClaim(t, "destination", 1)
	other := cacheFixtureClaim(t, "other", 1)
	if _, err := RememberClaim(context.Background(), dir, other.ID, claim); !errors.Is(err, ErrTarget) {
		t.Fatalf("foreign declaration accepted: %v", err)
	}
	for _, next := range []Claim{
		{ID: claim.ID, MachineName: "UPPER", Revision: 1},
		{ID: claim.ID, MachineName: "list", Revision: 1},
		{ID: claim.ID, MachineName: "7k3d", Revision: 1},
		{ID: claim.ID, MachineName: "destination", Revision: 0},
		{ID: "../../outside", MachineName: "destination", Revision: 1},
	} {
		if _, err := RememberClaim(context.Background(), dir, next.ID, next); err == nil {
			t.Fatalf("malformed declaration accepted: %+v", next)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RememberClaim(ctx, dir, claim.ID, claim); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled declaration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, cacheDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected input wrote cache state: %v", err)
	}
}

func TestClaimCacheConcurrentWritersKeepHighestRevision(t *testing.T) {
	dir := t.TempDir()
	claim := cacheFixtureClaim(t, "destination", 1)
	other := cacheFixtureClaim(t, "other", 1)
	if _, err := RememberClaim(context.Background(), dir, other.ID, other); err != nil {
		t.Fatal(err)
	}
	var writers sync.WaitGroup
	for revision := uint64(1); revision <= 8; revision++ {
		writers.Go(func() {
			next := claim
			next.Revision = revision
			_, err := RememberClaim(context.Background(), dir, next.ID, next)
			if err != nil && !errors.Is(err, ErrReplay) {
				t.Errorf("concurrent claim revision %d: %v", revision, err)
			}
		})
	}
	writers.Wait()
	got, err := CachedClaim(dir, claim.ID)
	if err != nil || got.Revision != 8 {
		t.Fatalf("highest committed claim lost: %+v err=%v", got, err)
	}
	got, err = CachedClaim(dir, other.ID)
	if err != nil || got != other {
		t.Fatalf("independent destination changed: %+v err=%v", got, err)
	}
}

func TestClaimCacheSeparateProcessHonorsWriterLock(t *testing.T) {
	if os.Getenv("MESH_NAMING_CACHE_CHILD") == "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
		defer cancel()
		claim := Claim{ID: os.Getenv("MESH_NAMING_CACHE_ID"), MachineName: "destination", Revision: 1}
		_, err := RememberClaim(ctx, os.Getenv("MESH_NAMING_CACHE_DIR"), claim.ID, claim)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("another process bypassed the held cache lock: %v", err)
		}
		return
	}
	dir := t.TempDir()
	claim := cacheFixtureClaim(t, "destination", 1)
	if _, err := RememberClaim(context.Background(), dir, claim.ID, claim); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, cacheDirectory, claim.ID+".lock"), os.O_RDWR, 0) //nolint:gosec // private fixture lock
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // fixture descriptor cleanup releases lock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestClaimCacheSeparateProcessHonorsWriterLock$") //nolint:gosec // executes this test's own binary
	command.Env = append(os.Environ(), "MESH_NAMING_CACHE_CHILD=1", "MESH_NAMING_CACHE_DIR="+dir, "MESH_NAMING_CACHE_ID="+claim.ID)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cache lock process control: %v\n%s", err, output)
	}
}

func TestClaimCacheInvalidDiskStateNeverReplaced(t *testing.T) {
	for _, mode := range []string{"symlink", "permissions", "foreign", "unknown", "trailing", "oversize", "receipt", "lock-permissions"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			claim := cacheFixtureClaim(t, "destination", 1)
			if _, err := RememberClaim(context.Background(), dir, claim.ID, claim); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, cacheDirectory, claim.ID+".json")
			contents, err := os.ReadFile(path) //nolint:gosec // private fixture state
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "symlink":
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".original", path); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // deliberately unsafe permissions test cache rejection
					t.Fatal(err)
				}
			case "foreign":
				contents = []byte(strings.ReplaceAll(string(contents), claim.ID, cacheFixtureClaim(t, "other", 1).ID))
			case "unknown":
				contents = []byte(strings.Replace(string(contents), "{", "{\"another\":1,", 1))
			case "trailing":
				contents = append(contents, []byte("{}")...)
			case "oversize":
				contents = []byte(strings.Repeat(" ", maximumStateBytes+1))
			case "receipt":
				contents = []byte(strings.Replace(string(contents), "{", "{\"previousRevision\":1,", 1))
			case "lock-permissions":
				if err := os.Chmod(filepath.Join(dir, cacheDirectory, claim.ID+".lock"), 0o644); err != nil { //nolint:gosec // deliberately unsafe permissions test writer rejection
					t.Fatal(err)
				}
			}
			if mode != "symlink" && mode != "permissions" && mode != "lock-permissions" {
				writeInvalidCacheFixture(t, path, contents)
			}
			claim.Revision = 2
			if _, err := RememberClaim(context.Background(), dir, claim.ID, claim); err == nil {
				t.Fatalf("invalid cached state replaced in mode %s", mode)
			}
		})
	}
}

func TestClaimPartitionsConvergeWithDeterministicConflicts(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	a := cacheFixtureClaim(t, "shared", 1)
	b := cacheFixtureClaim(t, "shared", 1)
	for _, pair := range []struct {
		dir   string
		claim Claim
	}{{left, a}, {right, b}} {
		if _, err := RememberClaim(context.Background(), pair.dir, pair.claim.ID, pair.claim); err != nil {
			t.Fatal(err)
		}
		if rows := Project([]Claim{pair.claim}); len(rows) != 1 || rows[0].Conflict {
			t.Fatal("invented an unseen partition conflict")
		}
	}
	for _, dir := range []string{left, right} {
		for _, claim := range []Claim{a, b} {
			if _, err := RememberClaim(context.Background(), dir, claim.ID, claim); err != nil {
				t.Fatal(err)
			}
		}
	}
	one, two := Project([]Claim{a, b}), Project([]Claim{b, a})
	if !reflect.DeepEqual(one, two) || !one[0].Conflict || !one[1].Conflict || !one[0].Priority || one[1].Priority || one[0].ID > one[1].ID {
		t.Fatalf("nonconvergent conflict projection: %+v / %+v", one, two)
	}
	b.MachineName, b.Revision = "renamed", 2
	if _, err := RememberClaim(context.Background(), left, b.ID, b); err != nil {
		t.Fatal(err)
	}
	old, err := CachedClaim(right, b.ID)
	if err != nil || old.Revision != 1 {
		t.Fatalf("offline owner claim invented: %+v %v", old, err)
	}
	if _, err := RememberClaim(context.Background(), right, b.ID, b); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{left, right} {
		current, err := CachedClaim(dir, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		rows := Project([]Claim{current, a})
		if rows[0].Conflict || rows[1].Conflict || current != b {
			t.Fatalf("rename did not converge: %+v", rows)
		}
	}
}

func writeInvalidCacheFixture(t *testing.T, path string, contents []byte) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close() //nolint:errcheck // fixture directory descriptor cleanup
	if err := root.WriteFile(filepath.Base(path), contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaimCacheCreatesNestedConfigurationAndFollowsSelectedRoot(t *testing.T) {
	for _, mode := range []string{"nested", "selected-symlink"} {
		t.Run(mode, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "new", "configuration")
			if mode == "selected-symlink" {
				directory = filepath.Join(t.TempDir(), "selected")
				if err := os.Symlink(t.TempDir(), directory); err != nil {
					t.Fatal(err)
				}
			}
			claim := cacheFixtureClaim(t, "destination", 1)
			if _, err := RememberClaim(t.Context(), directory, claim.ID, claim); err != nil {
				t.Fatal(err)
			}
			if cached, err := CachedClaim(directory, claim.ID); err != nil || cached != claim {
				t.Fatalf("configured root failed to retain its claim: %+v %v", cached, err)
			}
		})
	}
}
