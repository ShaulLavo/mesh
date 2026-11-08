package access

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func testSubject(t *testing.T) Subject {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Subject(base64.RawURLEncoding.EncodeToString(public))
}
func testStore(t *testing.T) (*Store, Subject, time.Time) {
	t.Helper()
	store, err := Open(t.TempDir(), string(testSubject(t)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return store, testSubject(t), time.Now().UTC().Add(time.Hour)
}
func noRoles(context.Context, Subject) (OtherRoles, error) { return OtherRoles{}, nil }
func testGrant(t *testing.T, store *Store, revision uint64, subject Subject, expiry time.Time) Change {
	t.Helper()
	result, err := store.Grant(t.Context(), revision, GrantInput{Permissions: []string{"metadata"}, Subject: subject, ExpiresAt: expiry}, noRoles)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestScopedPolicyLifecycleAndSnapshotOwnership(t *testing.T) {
	store, subject, expiry := testStore(t)
	policy, err := store.List(t.Context())
	if err != nil || policy.Revision != 0 || policy.Entries == nil {
		t.Fatalf("absence: %+v %v", policy, err)
	}
	if _, err := os.Stat(filepath.Join(store.stateDir, "access")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read initialized policy directory")
	}
	first := testGrant(t, store, 0, subject, expiry)
	binding, err := store.Bind(subject)
	if err != nil || !binding.Current() {
		t.Fatal("grant did not bind", err)
	}
	same := testGrant(t, store, 0, subject, expiry)
	if same.Changed || same.Entry.Incarnation != first.Entry.Incarnation || !binding.Current() {
		t.Fatal("exact retry replaced entry")
	}
	other := testSubject(t)
	testGrant(t, store, 1, other, expiry)
	if !binding.Current() {
		t.Fatal("unrelated grant retired binding")
	}
	policy, err = store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	policy.Entries[0].Permissions[0] = "forged"
	first.Entry.Permissions[0] = "forged"
	if !binding.Current() || binding.Principal().Permissions()[0] != "metadata" {
		t.Fatal("caller mutated published binding")
	}
	if _, err := store.Revoke(t.Context(), 2, subject); err != nil {
		t.Fatal(err)
	}
	if binding.Current() {
		t.Fatal("revoked binding remained current")
	}
	testGrant(t, store, 3, subject, expiry)
	if binding.Current() {
		t.Fatal("regrant healed old binding")
	}
	fresh, err := store.Bind(subject)
	if err != nil || !fresh.Current() {
		t.Fatal("new grant did not bind", err)
	}
	testGrant(t, store, 4, subject, expiry.Add(time.Hour))
	if fresh.Current() {
		t.Fatal("expiry replacement did not retire binding")
	}
}
func TestExpiredRegistrationAndInvalidationLatch(t *testing.T) {
	store, subject, expiry := testStore(t)
	now := expiry.Add(-time.Hour)
	store.now = func() time.Time { return now }
	testGrant(t, store, 0, subject, expiry)
	binding, err := store.Bind(subject)
	if err != nil {
		t.Fatal(err)
	}
	now = expiry
	if binding.Current() {
		t.Fatal("expired binding current")
	}
	registered, err := store.Membership(subject)
	if err != nil || !registered {
		t.Fatal("expiry removed registration", err)
	}
	now = expiry.Add(-time.Minute)
	if binding.Current() {
		t.Fatal("backward wall clock healed invalid binding")
	}
	if _, err := store.Bind(subject); err != nil {
		t.Fatal("fresh handshake must use current target wall clock", err)
	}
}
func TestScopedPolicySafetyAndCacheInvalidation(t *testing.T) {
	for _, kind := range []string{"corrupt", "unknown", "duplicate", "field-alias", "symlink", "hardlink", "fifo", "oversize", "permissions", "wrong-target"} {
		t.Run(kind, func(t *testing.T) { testUnsafeSnapshot(t, kind) })
	}
}
func testUnsafeSnapshot(t *testing.T, kind string) {
	t.Helper()
	store, subject, expiry := testStore(t)
	testGrant(t, store, 0, subject, expiry)
	binding, err := store.Bind(subject)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(store.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	path := "access/scopes.json"
	data, err := root.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Remove(path); err != nil {
		t.Fatal(err)
	}
	replacement := data
	switch kind {
	case "corrupt":
		replacement = []byte("{")
	case "unknown":
		replacement = append([]byte(`{"unknown":1,`), data[1:]...)
	case "duplicate":
		replacement = append([]byte(`{"version":1,`), data[1:]...)
	case "field-alias":
		replacement = []byte(strings.Replace(string(data), `"permissions":["metadata"]`, `"permissions":["session-output"],"Permissions":["metadata"]`, 1))
	case "oversize":
		replacement = make([]byte, MaximumPolicy+1)
	case "wrong-target":
		var policy PolicyView
		if err := json.Unmarshal(data, &policy); err != nil {
			t.Fatal(err)
		}
		policy.Target = string(testSubject(t))
		replacement, err = json.Marshal(policy)
	}
	if err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "symlink":
		target := "target"
		if err := root.WriteFile(target, data, 0600); err != nil {
			t.Fatal(err)
		}
		err = root.Symlink("../target", path)
	case "hardlink":
		target := "target"
		if err := root.WriteFile(target, data, 0600); err != nil {
			t.Fatal(err)
		}
		err = root.Link(target, path)
	case "fifo":
		err = unix.Mkfifo(filepath.Join(root.Name(), path), 0600)
	default:
		err = root.WriteFile(path, replacement, 0600)
	}
	if err != nil {
		t.Fatal(err)
	}
	if kind == "permissions" {
		if err := root.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.List(t.Context()); err == nil {
		t.Fatal("unsafe policy read as valid")
	}
	if binding.Current() {
		t.Fatal("last-good cache authorized unsafe policy")
	}
	if err := root.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if binding.Current() {
		t.Fatal("repair healed invalidated binding")
	}
}
func TestScopePublicationFailureRepairsWithoutNewIncarnation(t *testing.T) {
	store, subject, expiry := testStore(t)
	cause := errors.New("directory barrier failed")
	store.syncDir = func(*os.File) error { return cause }
	change, err := store.Grant(t.Context(), 0, GrantInput{Permissions: []string{"metadata"}, Subject: subject, ExpiresAt: expiry}, noRoles)
	var visible *VisibleUnconfirmed
	if !errors.As(err, &visible) || !errors.Is(err, cause) || visible.Revision != 1 || change.Changed {
		t.Fatalf("unconfirmed outcome: %+v %v", change, err)
	}
	first := change.Entry.Incarnation
	calls := 0
	store.syncDir = func(file *os.File) error { calls++; return file.Sync() }
	retry := testGrant(t, store, 0, subject, expiry)
	if retry.Changed || retry.Entry.Incarnation != first || calls != 2 {
		t.Fatal("retry did not settle exact visible policy")
	}
	binding, err := store.Bind(subject)
	if err != nil {
		t.Fatal(err)
	}
	store.syncDir = func(*os.File) error { return cause }
	_, err = store.Revoke(t.Context(), 1, subject)
	if !errors.As(err, &visible) || binding.Current() {
		t.Fatal("unconfirmed revoke was rolled back")
	}
	store.syncDir = (*os.File).Sync
	if _, err := store.Revoke(t.Context(), 1, subject); err != nil {
		t.Fatal(err)
	}
}
func TestScopeWriterDeadlineAndPermanentInode(t *testing.T) {
	store, subject, expiry := testStore(t)
	testGrant(t, store, 0, subject, expiry)
	path := filepath.Join(store.stateDir, "access", "key-classes.lock")
	root, err := os.OpenRoot(store.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile("access/key-classes.lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.Revoke(ctx, 1, subject); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := store.Revoke(ctx, 1, subject); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Revoke(t.Context(), 1, subject); err != nil {
		t.Fatal(err)
	}
	next, err := os.Stat(path)
	if err != nil || !os.SameFile(info, next) {
		t.Fatal("writer replaced permanent lock", err)
	}
}
func TestScopeCASAndExhaustion(t *testing.T) {
	store, subject, expiry := testStore(t)
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wait.Go(func() {
			_, err := store.Grant(t.Context(), 0, GrantInput{Permissions: []string{"metadata"}, Subject: subject, ExpiresAt: expiry}, noRoles)
			results <- err
		})
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Grant(t.Context(), 0, GrantInput{Permissions: []string{"metadata"}, Subject: subject, ExpiresAt: expiry.Add(time.Hour)}, noRoles); !errors.Is(err, ErrRevision) {
		t.Fatalf("CAS: %v", err)
	}
	policy, err := store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	policy.Revision = math.MaxUint64
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.stateDir, "access", "scopes.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Revoke(t.Context(), math.MaxUint64, subject); err == nil {
		t.Fatal("revision wrapped")
	}
}

func TestScopedMonotonicCapSurvivesBackwardWallStep(t *testing.T) {
	store, subject, expiry := testStore(t)
	wall, monotonic := expiry.Add(-time.Hour), time.Now()
	store.now = func() time.Time { return wall }
	store.monoNow = func() time.Time { return monotonic }
	testGrant(t, store, 0, subject, expiry)
	binding, err := store.Bind(subject)
	if err != nil {
		t.Fatal(err)
	}
	wall = wall.Add(-24 * time.Hour)
	monotonic = monotonic.Add(time.Hour)
	if binding.Current() {
		t.Fatal("backward wall step extended a bound socket beyond its monotonic cap")
	}
}

func TestScopeParentBarrierFailureRemainsVisibleUnconfirmed(t *testing.T) {
	store, subject, expiry := testStore(t)
	calls := 0
	cause := errors.New("parent barrier failed")
	store.syncDir = func(file *os.File) error {
		calls++
		if calls == 2 {
			return cause
		}
		return file.Sync()
	}
	_, err := store.Grant(t.Context(), 0, GrantInput{Permissions: []string{"metadata"}, Subject: subject, ExpiresAt: expiry}, noRoles)
	var visible *VisibleUnconfirmed
	if !errors.As(err, &visible) || !errors.Is(err, cause) || visible.Revision != 1 {
		t.Fatalf("parent barrier acknowledged: %v", err)
	}
	policy, err := store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	generation := policy.Entries[0].Incarnation
	store.syncDir = (*os.File).Sync
	result := testGrant(t, store, 0, subject, expiry)
	if result.Changed || result.Entry.Incarnation != generation {
		t.Fatal("parent repair allocated another incarnation")
	}
}
func TestScopeCancellationAfterPublicationKeepsRequiredBarriers(t *testing.T) {
	store, subject, expiry := testStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	store.syncDir = func(file *os.File) error { calls++; cancel(); return file.Sync() }
	change, err := store.Grant(ctx, 0, GrantInput{Permissions: []string{"metadata"}, Subject: subject, ExpiresAt: expiry}, noRoles)
	if err != nil || !change.Changed || calls != 2 {
		t.Fatalf("accepted publication lost barriers after cancellation: %+v %v calls=%d", change, err, calls)
	}
}
