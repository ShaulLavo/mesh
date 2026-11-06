package hostprofile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func editable(text string) Editable { return Editable{SchemaVersion: 1, Instructions: text} }

func TestConcurrentCAS(t *testing.T) {
	for _, identical := range []bool{false, true} {
		t.Run(map[bool]string{false: "different", true: "identical"}[identical], func(t *testing.T) { concurrentCAS(t, identical) })
	}
}

func concurrentCAS(t *testing.T, identical bool) {
	t.Helper()
	dir := t.TempDir()
	var publications atomic.Int32
	stores := []*HostStore{OpenHostStore(dir, "host"), OpenHostStore(dir, "host")}
	for _, store := range stores {
		store.rename = func(dir *os.File, name string) error {
			publications.Add(1)
			return unix.Renameat(int(dir.Fd()), name, int(dir.Fd()), "profile.json")
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for i, store := range stores {
		group.Go(func() {
			<-start
			text := "one"
			if i == 1 && !identical {
				text = "two"
			}
			_, _, err := store.Set(t.Context(), 0, editable(text))
			results <- err
		})
	}
	close(start)
	group.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		var conflict *ConflictError
		if !errors.As(err, &conflict) {
			t.Fatal(err)
		}
		conflicts++
	}
	want := 1
	if identical {
		want = 2
	}
	if successes != want || conflicts != 2-want || publications.Load() != 1 {
		t.Fatalf("success=%d conflicts=%d publications=%d", successes, conflicts, publications.Load())
	}
}

func TestPublicationFailuresAndRetry(t *testing.T) {
	for _, stage := range []string{"write", "file-sync", "rename", "directory-sync"} {
		t.Run(stage, func(t *testing.T) { publicationFailure(t, stage) })
	}
}

func publicationFailure(t *testing.T, stage string) {
	t.Helper()
	dir := t.TempDir()
	store := OpenHostStore(dir, "host")
	old, _, err := store.Set(t.Context(), 0, editable("old"))
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G304 -- Task-owned fixture path.
	original, err := os.ReadFile(filepath.Join(dir, "profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	injected := unix.ENOSPC
	switch stage {
	case "write":
		store.write = func(file *os.File, data []byte) error {
			if err := writeAll(file, data[:len(data)/2]); err != nil {
				return err
			}
			return injected
		}
	case "file-sync":
		store.syncFile = func(*os.File) error { return injected }
	case "rename":
		store.rename = func(*os.File, string) error { return injected }
	case "directory-sync":
		store.syncDir = func(*os.File) error { return injected }
	}
	if _, _, err := store.Set(t.Context(), old.Revision, editable("new")); !errors.Is(err, injected) {
		t.Fatalf("missing original failure: %v", err)
	}
	// #nosec G304 -- Task-owned fixture path.
	current, err := os.ReadFile(filepath.Join(dir, "profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	if stage != "directory-sync" && string(current) != string(original) {
		t.Fatal("pre-rename failure changed old bytes")
	}
	if stage == "directory-sync" {
		settleRetry(t, dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("temporary files leaked: %v", entries)
	}
}

func settleRetry(t *testing.T, dir string) {
	t.Helper()
	retry := OpenHostStore(dir, "host")
	calls := 0
	retry.syncFile = func(file *os.File) error { calls++; return file.Sync() }
	retry.syncDir = func(file *os.File) error { calls++; return file.Sync() }
	current, changed, err := retry.Set(t.Context(), 1, editable("new"))
	if err != nil || changed || current.Revision != 2 || calls != 2 {
		t.Fatalf("retry: rev=%d changed=%t barriers=%d err=%v", current.Revision, changed, calls, err)
	}
}

func TestAbsentClearIdentityAndExhaustion(t *testing.T) {
	dir := t.TempDir()
	store := OpenHostStore(dir, "first")
	absent, err := store.Read()
	if err != nil || absent.State != StateAbsent || absent.Revision == nil || *absent.Revision != 0 {
		t.Fatalf("absent %+v %v", absent, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("read created files")
	}
	first, changed, err := store.Set(t.Context(), 0, editable("same"))
	if err != nil || !changed || first.Revision != 1 {
		t.Fatal(first, changed, err)
	}
	other := OpenHostStore(dir, "second")
	wrong, err := other.Read()
	if err != nil || wrong.State != StateInvalid || wrong.Revision == nil || *wrong.Revision != 1 {
		t.Fatal(wrong, err)
	}
	if _, _, err := other.Set(t.Context(), 0, editable("same")); err == nil {
		t.Fatal("same content acknowledged another identity")
	}
	replaced, _, err := other.Set(t.Context(), 1, editable("same"))
	if err != nil || replaced.Revision != 2 || replaced.HostID != "second" {
		t.Fatal(replaced, err)
	}
	cleared, _, err := other.Set(t.Context(), 2, editable(""))
	if err != nil || cleared.Revision != 3 {
		t.Fatal(cleared, err)
	}
	cleared.Revision = math.MaxUint64
	writeStored(t, dir, cleared)
	if _, _, err := other.Set(t.Context(), math.MaxUint64, editable("next")); err == nil {
		t.Fatal("revision wrapped")
	}
	snapshot, err := other.Read()
	if err != nil || *snapshot.Revision != math.MaxUint64 {
		t.Fatal(snapshot, err)
	}
}

func writeStored(t *testing.T, dir string, p HostProfile) {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDamagedStatePreserved(t *testing.T) {
	for _, value := range []string{`{"revision":"bad"}`, `{"schemaVersion":1,"revision":4,"revision":5}`, `{`, `null`} {
		dir := t.TempDir()
		path := filepath.Join(dir, "profile.json")
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		store := OpenHostStore(dir, "host")
		snapshot, _ := store.Read()
		if snapshot.State != StateInvalid || snapshot.Revision != nil {
			t.Fatal(snapshot)
		}
		if _, _, err := store.Set(t.Context(), 0, editable("next")); err == nil {
			t.Fatal("damaged state overwritten")
		}
		// #nosec G304 -- Task-owned fixture path.
		after, _ := os.ReadFile(path)
		if string(after) != value {
			t.Fatal("damaged bytes changed")
		}
	}
}

func TestFileSafety(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "public", "fifo", "directory", "lock-symlink", "unsafe-dir"} {
		t.Run(kind, func(t *testing.T) { fileSafety(t, kind) })
	}
}

func fileSafety(t *testing.T, kind string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "profile.json")
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	switch kind {
	case "symlink":
		err = os.Symlink(target, path)
	case "hardlink":
		err = os.Link(target, path)
	case "public":
		// #nosec G306 -- Deliberately unsafe fixture permissions must be refused.
		err = os.WriteFile(path, []byte(`{}`), 0644)
	case "fifo":
		err = unix.Mkfifo(path, 0600)
	case "directory":
		err = os.Mkdir(path, 0700)
	case "lock-symlink":
		err = os.Symlink(target, filepath.Join(dir, ".profile.lock"))
	case "unsafe-dir":
		// #nosec G302 -- Deliberately unsafe fixture directory must be refused.
		err = os.Chmod(dir, 0770)

	}
	if err != nil {
		t.Fatal(err)
	}
	store := OpenHostStore(dir, "host")
	if _, _, err := store.Set(t.Context(), 0, editable("new")); err == nil {
		t.Fatal("unsafe write accepted")
	}
	// #nosec G304 -- Task-owned fixture target.
	after, _ := os.ReadFile(target)
	if string(after) != "keep" {
		t.Fatal("target modified")
	}
}

func TestLockBoundAndMissingDirectory(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	_, err := OpenHostStore(missing, "host").Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created directory")
	}
	// #nosec G304 -- Task-owned fixture lock.
	lock, err := os.OpenFile(filepath.Join(dir, ".profile.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := OpenHostStore(dir, "host").Set(ctx, 0, editable("new")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := OpenHostStore(dir, "host").Set(ctx, 0, editable("new")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestAnchoredDirectoryAndStrictFinalSymlink(t *testing.T) {
	root := t.TempDir()
	left := filepath.Join(root, "left")
	right := filepath.Join(root, "right")
	for _, path := range []string{left, right} {
		if err := os.MkdirAll(filepath.Join(path, "config"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(left, alias); err != nil {
		t.Fatal(err)
	}
	dir, err := openDirectory(filepath.Join(alias, "config"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(right, alias); err != nil {
		t.Fatal(err)
	}
	store := OpenHostStore(filepath.Join(alias, "config"), "host")
	if err := store.publish(dir, []byte("anchored")); err != nil {
		t.Fatal(err)
	}
	// #nosec G304 -- Both directories are owned by this fixture.
	data, err := os.ReadFile(filepath.Join(left, "config", "profile.json"))
	if err != nil || string(data) != "anchored" {
		t.Fatal(string(data), err)
	}
	if _, err := os.Lstat(filepath.Join(right, "config", "profile.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("publication followed retargeted ancestor")
	}
	if _, _, err := OpenHostStore(alias, "host").Set(t.Context(), 0, editable("next")); err == nil {
		t.Fatal("final symlink accepted")
	}
}

func TestRepeatedUnconfirmedRetryDoesNotIncrement(t *testing.T) {
	dir := t.TempDir()
	store := OpenHostStore(dir, "host")
	store.syncDir = func(*os.File) error { return unix.EIO }
	if _, _, err := store.Set(t.Context(), 0, editable("next")); !errors.Is(err, unix.EIO) {
		t.Fatal(err)
	}
	retry := OpenHostStore(dir, "host")
	retry.syncDir = func(*os.File) error { return unix.EIO }
	if _, _, err := retry.Set(t.Context(), 0, editable("next")); !errors.Is(err, unix.EIO) {
		t.Fatal("equality bypassed directory barrier", err)
	}
	retry.syncFile = func(*os.File) error { return unix.EIO }
	if _, _, err := retry.Set(t.Context(), 0, editable("next")); !errors.Is(err, unix.EIO) {
		t.Fatal("equality bypassed file barrier", err)
	}
	final, changed, err := OpenHostStore(dir, "host").Set(t.Context(), 0, editable("next"))
	if err != nil || changed || final.Revision != 1 {
		t.Fatal(final, changed, err)
	}
}

func TestPrivateFileOwnerAndReadSafety(t *testing.T) {
	// Foreign ownership is checked from the same fstat shape without privileged chown.
	stat := unix.Stat_t{Mode: unix.S_IFREG | 0600, Uid: uint32(os.Geteuid()), Nlink: 1} //nolint:gosec // Unix effective UID representation.
	if err := validatePrivateStat(stat); err != nil {
		t.Fatal(err)
	}
	stat.Uid ^= 1
	if err := validatePrivateStat(stat); err == nil {
		t.Fatal("foreign owner accepted")
	}
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte(`{"schemaVersion":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "profile.json")); err != nil {
		t.Fatal(err)
	}
	snapshot, err := OpenHostStore(dir, "host").Read()
	if err == nil || snapshot.State != StateUnavailable || snapshot.Revision != nil || snapshot.Value != nil {
		t.Fatal(snapshot, err)
	}
}

func TestInvalidProblemIsBoundedAndControlFree(t *testing.T) {
	dir := t.TempDir()
	data, err := json.Marshal(map[string]string{string(make([]byte, 1024)): "value"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := OpenHostStore(dir, "host").Read()
	if err != nil || snapshot.State != StateInvalid || len(snapshot.Problem) > 512 || len(snapshot.Problem) == 0 {
		t.Fatal(snapshot, err)
	}
	for _, r := range snapshot.Problem {
		if r < 32 {
			t.Fatal("untrusted field leaked a control character")
		}
	}
}

func TestDefaultLockBudgetExpiresBeforeCaller(t *testing.T) {
	dir, err := openDirectory(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	held, err := acquireLock(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	caller, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	waiting, err := acquireLock(caller, dir)
	if waiting != nil || !errors.Is(err, context.DeadlineExceeded) || caller.Err() != nil {
		t.Fatalf("internal lock budget did not end the wait: file=%v err=%v caller=%v", waiting, err, caller.Err())
	}
}

func TestOversizedStoredFilePreservesBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profile.json")
	data := bytes.Repeat([]byte(" "), HostLimits.Encoded+1)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	store := OpenHostStore(dir, "host")
	snapshot, err := store.Read()
	if err != nil || snapshot.State != StateInvalid || snapshot.Revision != nil {
		t.Fatal(snapshot, err)
	}
	if _, _, err := store.Set(t.Context(), 0, editable("next")); err == nil {
		t.Fatal("oversized state overwritten")
	}
	// #nosec G304 -- Task-owned oversized fixture.
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatal("oversized state changed", err)
	}
}
