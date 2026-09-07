package updatenotice

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
)

func testManifest(version string) release.Manifest {
	manifest := release.Manifest{
		Schema: 1, Version: version, Commit: strings.Repeat("a", 40),
		Compatibility: release.Compatibility{StateReadMin: 1, StateReadMax: 1, StateWrite: 1, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1},
	}
	for _, platform := range []release.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "arm64"}} {
		manifest.Artifacts = append(manifest.Artifacts, release.Artifact{Platform: platform, Archive: "mesh_" + platform.OS + "_" + platform.Arch + ".tar.gz", SHA256: strings.Repeat("b", 64), BinarySHA256: strings.Repeat("c", 64)})
	}
	return manifest
}

func testStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	store := New(Config{Directory: filepath.Join(t.TempDir(), "notice"), Current: release.Build{Version: "v0.1.0"}})
	store.now = func() time.Time { return now }
	store.jitter = func() time.Duration { return 7 * time.Minute }
	store.fetch = func(context.Context) (release.Manifest, error) { return testManifest("v0.2.0"), nil }
	return store, &now
}

func TestConstructionAndCachedReadsHaveNoSideEffects(t *testing.T) {
	store, _ := testStore(t)
	store.fetch = func(context.Context) (release.Manifest, error) {
		t.Fatal("cached read fetched a release")
		return release.Manifest{}, nil
	}
	if notice, err := store.Cached(); err != nil || notice.Version != "" {
		t.Fatalf("empty cache = %+v, %v", notice, err)
	}
	if _, err := os.Stat(store.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("constructor or cached read created directory: %v", err)
	}
}

func TestSuccessfulChecksPersistTTLAndJitter(t *testing.T) {
	store, now := testStore(t)
	first, err := store.Refresh(context.Background(), false)
	if err != nil || first.Version != "v0.2.0" || !first.CheckedAt.Equal(*now) {
		t.Fatalf("first refresh = %+v, %v", first, err)
	}
	store.fetch = func(context.Context) (release.Manifest, error) {
		t.Fatal("fresh cache fetched again")
		return release.Manifest{}, nil
	}
	*now = now.Add(6*time.Hour + 6*time.Minute)
	if _, err := store.Refresh(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	state, err := store.read()
	if err != nil || !state.NextAttempt.Equal(first.CheckedAt.Add(6*time.Hour+7*time.Minute)) {
		t.Fatalf("persisted deadline = %v, %v", state.NextAttempt, err)
	}
	for _, name := range []string{"notice.json", "refresh.lock", "state.lock"} {
		info, err := os.Stat(filepath.Join(store.directory, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s permissions: %v, %v", name, info, err)
		}
	}
}

func TestFailedCheckKeepsPreviousResultAndThrottlesAcrossInstances(t *testing.T) {
	store, now := testStore(t)
	first, err := store.Refresh(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(7 * time.Hour)
	outage := errors.New("release server unavailable")
	store.fetch = func(context.Context) (release.Manifest, error) { return release.Manifest{}, outage }
	got, err := store.Refresh(context.Background(), false)
	if !errors.Is(err, outage) || got != first {
		t.Fatalf("failed refresh discarded cached result: %+v, %v", got, err)
	}
	reopened := New(Config{Directory: store.directory, Current: store.current})
	reopened.now = store.now
	reopened.fetch = func(context.Context) (release.Manifest, error) {
		t.Fatal("outage retry was not throttled")
		return release.Manifest{}, nil
	}
	if got, err := reopened.Refresh(context.Background(), false); err != nil || got != first {
		t.Fatalf("reopened cache = %+v, %v", got, err)
	}
	state, err := store.read()
	if err != nil || !state.AttemptedAt.Equal(*now) || !state.CheckedAt.Equal(first.CheckedAt) {
		t.Fatalf("attempt overwrote successful timestamp: %+v, %v", state, err)
	}
	store.fetch = func(context.Context) (release.Manifest, error) { return testManifest("v0.3.0"), nil }
	if got, err := store.Refresh(context.Background(), true); err != nil || got.Version != "v0.3.0" {
		t.Fatalf("explicit refresh did not bypass throttle: %+v, %v", got, err)
	}
}

func TestSnoozeAndSkipArePersistedPerVersion(t *testing.T) {
	store, now := testStore(t)
	if _, err := store.Refresh(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := store.Dismiss(context.Background(), "v0.2.0", false); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Cached(); got.Version != "" {
		t.Fatalf("snoozed notice = %+v", got)
	}
	*now = now.Add(24 * time.Hour)
	if got, _ := store.Cached(); got.Version != "v0.2.0" {
		t.Fatalf("expired snooze = %+v", got)
	}
	if err := store.Dismiss(context.Background(), "v0.2.0", true); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(48 * time.Hour)
	if got, _ := store.Cached(); got.Version != "" {
		t.Fatalf("skipped notice = %+v", got)
	}
	store.fetch = func(context.Context) (release.Manifest, error) { return testManifest("v0.3.0"), nil }
	if got, err := store.Refresh(context.Background(), true); err != nil || got.Version != "v0.3.0" {
		t.Fatalf("skipping previous release hid new release: %+v, %v", got, err)
	}
}

func TestRefreshLockDoesNotBlockAttachmentOrDismissal(t *testing.T) {
	store, _ := testStore(t)
	if _, err := store.Refresh(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	entered, finish := make(chan struct{}), make(chan struct{})
	store.fetch = func(context.Context) (release.Manifest, error) {
		close(entered)
		<-finish
		return testManifest("v0.2.0"), nil
	}
	done := make(chan error, 1)
	go func() { _, err := store.Refresh(context.Background(), true); done <- err }()
	<-entered
	defer close(finish)
	other := New(Config{Directory: store.directory, Current: store.current})
	other.now = store.now
	other.fetch = func(context.Context) (release.Manifest, error) {
		t.Error("concurrent instance fetched")
		return release.Manifest{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if got, err := other.Refresh(ctx, true); err != nil || got.Version != "v0.2.0" {
		t.Fatalf("concurrent refresh waited instead of reading cache: %+v, %v", got, err)
	}
	if err := other.Dismiss(ctx, "v0.2.0", true); err != nil {
		t.Fatalf("network request blocked dismissal: %v", err)
	}
	// Finish before test cleanup without closing the release signal twice.
	finish <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, _ := other.Cached(); got.Version != "" {
		t.Fatalf("refresh overwrote concurrent dismissal: %+v", got)
	}
}

func TestRefreshBoundsDeadlineAndPersistsCancelledAttempt(t *testing.T) {
	store, _ := testStore(t)
	store.fetch = func(ctx context.Context) (release.Manifest, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > checkTimeout {
			t.Errorf("unbounded check: %v, %v", deadline, ok)
		}
		<-ctx.Done()
		return release.Manifest{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := store.Refresh(ctx, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled refresh = %v", err)
	}
	state, err := store.read()
	if err != nil || state.AttemptedAt.IsZero() || !state.CheckedAt.IsZero() {
		t.Fatalf("cancelled attempt was not persisted: %+v, %v", state, err)
	}
}

func TestNoDowngradeOrDevelopmentNotice(t *testing.T) {
	store, _ := testStore(t)
	if _, err := store.Refresh(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"v0.2.0", "v0.3.0", "", "devel"} {
		store.current.Version = version
		if got, err := store.Cached(); err != nil || got.Version != "" {
			t.Fatalf("current %q received update notice: %+v, %v", version, got, err)
		}
	}
}

func TestInvalidManifestNeverBecomesNotice(t *testing.T) {
	store, _ := testStore(t)
	store.fetch = func(context.Context) (release.Manifest, error) { return release.Manifest{Version: "v9.0.0"}, nil }
	if got, err := store.Refresh(context.Background(), false); err == nil || got.Version != "" {
		t.Fatalf("incomplete release advertised: %+v, %v", got, err)
	}
}
