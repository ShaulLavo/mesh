package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/session"
)

// Stop is paused at the existing unlocked boundary just after acceptStop.
// A later Start must finish the accepted stop, then launch a successor.
func TestReviewKeptLiveStopAcceptanceOwesAStop(t *testing.T) {
	f := newKeptFixture(t)
	f.syncKept(t)
	if _, _, err := f.route().acceptStop(); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Start(t.Context(), "dev"); err != nil {
		t.Fatal(err)
	}
	settleDemand(t, f.manager)
	started, stopped := f.sessions.counts()
	if started != 2 || stopped != 1 {
		t.Fatalf("accepted live Stop then Start: started=%d stopped=%d status=%+v; want S001 stopped and one successor", started, stopped, f.manager.Status("dev"))
	}
}

// Required cleanup is already owed, but no cleanup goroutine is pending when
// Start is accepted. Its replacement must not depend on the waiter staying.
func TestReviewFiniteStartSurvivesWaiterLeavingItsCleanup(t *testing.T) {
	f := newKeptFixture(t)
	f.sessions.startErr = errors.New("launch returned an uncertain worker")
	f.manager.Sync([]meshserve.Service{demandService(time.Hour)})
	if err := f.manager.Start(t.Context(), "dev"); err == nil {
		t.Fatal("fixture start unexpectedly succeeded")
	}
	settleDemand(t, f.manager)
	stopping := newDemandBarrier(t)
	f.sessions.mu.Lock()
	f.sessions.startErr = nil
	f.sessions.stopGate = stopping
	f.sessions.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	start := beginAdoptionCall(t, func(context.Context) error { return f.manager.Start(ctx, "dev") })
	awaitDemandSignal(t, stopping.entered, "required cleanup")
	cancel()
	if err := start.wait(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start = %v, want its waiter cancelled", err)
	}
	stopping.unblock()
	settleDemand(t, f.manager)
	started, stopped := f.sessions.counts()
	if started != 2 || stopped != 1 || f.sessions.liveCount() != 1 {
		t.Fatalf("accepted Start lost after cleanup: started=%d stopped=%d live=%d status=%+v; want one successor", started, stopped, f.sessions.liveCount(), f.manager.Status("dev"))
	}
}

// The runtime barrier fires only after the real Recover holds its source
// lock. It then pauses before any recovery marker is saved.
type reviewRecoveryRuntime struct{ gate *demandBarrier }

func (r reviewRecoveryRuntime) Inspect(ctx context.Context, _ string) (recovery.Source, error) {
	r.gate.enter.Do(func() { close(r.gate.entered) })
	select {
	case <-ctx.Done():
		return recovery.Source{}, fmt.Errorf("recovery fixture inspection: %w", ctx.Err())
	case <-r.gate.resume:
		return recovery.Source{}, errors.New("review fixture finished before recovery reads source")
	}
}

func (reviewRecoveryRuntime) Launch(context.Context, recovery.Launch) error {
	return errors.New("review fixture must not launch")
}

func (reviewRecoveryRuntime) ConfigureCommand(context.Context, string, *recovery.Command) error {
	return errors.New("review fixture must not configure a command")
}

func reviewLockOpenCount(path string) int {
	wanted, err := os.Stat(path)
	if err != nil {
		return 0
	}
	entries, _ := os.ReadDir("/proc/self/fd")
	count := 0
	for _, entry := range entries {
		opened, err := os.Stat(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && os.SameFile(wanted, opened) {
			count++
		}
	}
	return count
}

func awaitReviewRecoveryLockOpen(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for reviewLockOpenCount(path) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("real Recover did not open the automatic claim's lock inode")
		}
		time.Sleep(time.Millisecond)
	}
}

// Recovery opens the existing lock before polling flock on that same fd.
// A failed automatic retirement must not make its waiting fd invisible to
// the next pruning pass by unlinking and replacing the lock inode.
func TestReviewPrunePreservesARecoveryWaiterAfterFailedRetirement(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("the lock witness observes its own Linux file descriptors")
	}
	var store *claimingStore
	h := newHistoryCatalog(t, func(inner CatalogStore) CatalogStore {
		store = &claimingStore{CatalogStore: inner, fail: "H011"}
		return store
	})
	historyShorts(t, h, 11)
	h.seed(t)
	dir := filepath.Join(h.root, "H011")
	gate := newDemandBarrier(t)
	cfg := recovery.Config{SessionsDir: h.root, HostID: string(h.catalog.host.ID), Runtime: reviewRecoveryRuntime{gate: gate}}
	var joinRecovery func(*testing.T) error
	store.beforeFailure = func() {
		call := beginAdoptionCall(t, func(ctx context.Context) error { return runReviewRecovery(ctx, cfg) })
		joinRecovery = call.wait
		awaitReviewRecoveryLockOpen(t, filepath.Join(dir, "recovery.lock"))
	}
	if removed, err := h.prune(t, ""); err == nil || len(removed) != 0 {
		t.Fatalf("failed durable retirement = %v, %v", removed, err)
	}
	h.assertKept(t, "H011")
	awaitDemandSignal(t, gate.entered, "real recovery owning the source lock")
	store.fail = ""
	removed, err := h.prune(t, "")
	gate.unblock()
	_ = joinRecovery(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("pruned %v while real Recover owns the source lock; want its row and directory preserved", removed)
	}
	h.assertKept(t, "H011")
}

func TestReviewLiveStopAndSuccessorSurviveBothWaitersLeaving(t *testing.T) {
	f := newKeptFixture(t)
	f.syncKept(t)
	stopping := newDemandBarrier(t)
	f.sessions.mu.Lock()
	f.sessions.stopGate = stopping
	f.sessions.mu.Unlock()
	stopCtx, cancelStop := context.WithCancel(t.Context())
	t.Cleanup(cancelStop)
	stop := beginAdoptionCall(t, func(context.Context) error { return f.manager.Stop(stopCtx, "dev") })
	awaitDemandSignal(t, stopping.entered, "accepted live stop")
	startCtx, cancelStart := context.WithCancel(t.Context())
	t.Cleanup(cancelStart)
	start := beginAdoptionCall(t, func(context.Context) error { return f.manager.Start(startCtx, "dev") })
	awaitRoute(t, f.route(), "accepted successor", func(r *demandRoute) bool { return r.startRequested && r.lastStart > r.lastStop })
	cancelStop()
	cancelStart()
	if err := stop.wait(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop waiter = %v", err)
	}
	if err := start.wait(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start waiter = %v", err)
	}
	stopping.unblock()
	settleDemand(t, f.manager)
	f.assertStatus(t, protocol.DemandRunning, "S002", false)
	if started, stopped := f.sessions.counts(); started != 2 || stopped != 1 || f.sessions.liveCount() != 1 {
		t.Fatalf("after both waiters left: started=%d stopped=%d live=%d", started, stopped, f.sessions.liveCount())
	}
}

func runReviewRecovery(ctx context.Context, cfg recovery.Config) error {
	_, err := recovery.Recover(ctx, cfg, recovery.Request{SessionID: "H011"})
	if err != nil {
		return fmt.Errorf("recover review fixture: %w", err)
	}
	return nil
}

func TestReviewRecoveryLockObserverUsesFileIdentity(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("the lock witness observes its own Linux file descriptors")
	}
	file, err := os.CreateTemp(t.TempDir(), "lock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	second, err := os.Open(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(file.Name(), alias); err != nil {
		t.Fatal(err)
	}
	if got := reviewLockOpenCount(file.Name()); got != 2 {
		t.Fatalf("physical lock observer = %d, want both owned descriptors", got)
	}
	if got := reviewLockOpenCount(alias); got != 2 {
		t.Fatalf("aliased lock observer = %d, want both owned descriptors", got)
	}
}

func (reviewRecoveryRuntime) Reserve(context.Context) (session.Reservation, error) {
	return session.Reservation{}, errors.New("review fixture must not reserve")
}
func (reviewRecoveryRuntime) Renew(context.Context, session.Reservation) (session.Reservation, error) {
	return session.Reservation{}, errors.New("review fixture must not renew")
}
