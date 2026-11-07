package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/session"
)

func journalFixture(t *testing.T) (*Journal, Key, Intent, []string) {
	t.Helper()
	root := t.TempDir()
	j, err := Open(root, "boot", "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	k := Key{Host: "host", Class: "local-account", Principal: "account", Opaque: "job"}
	i, err := Normalize(Intent{Kind: "pty", Command: []string{"/bin/true"}, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	return j, k, i, []string{"PATH=/bin"}
}
func reserveTest(t *testing.T, j *Journal, k Key, i Intent, env []string) (Receipt, bool, error) {
	t.Helper()
	return j.Reserve(t.Context(), k, i, func() (Binding, error) { return newTestBinding(t, j, k, i, env) })
}
func newTestBinding(t *testing.T, j *Journal, k Key, i Intent, env []string) (Binding, error) {
	t.Helper()
	id, err := session.NewID()
	if err != nil {
		return Binding{}, executionBoundaryError("journal_test.go newTestBinding", err)
	}
	ref, err := session.NewReservation(id, "boot", session.Execution)
	if err != nil {
		return Binding{}, executionBoundaryError("journal_test.go newTestBinding", err)
	}
	dir := filepath.Join("s", id)
	if err := j.files.Ensure(dir); err != nil {
		return Binding{}, err
	}
	if _, err := j.files.Publish(filepath.Join(dir, ".launching"), ref, true); err != nil {
		return Binding{}, err
	}
	if _, err := j.files.Publish(filepath.Join(dir, ".reservation"), ref, true); err != nil {
		return Binding{}, err
	}
	return NewBinding(k, i, ref, env), nil
}
func TestConcurrentEqualKeyElectsOnlyOneLaunchOwner(t *testing.T) {
	j, k, i, env := journalFixture(t)
	var owners, allocations atomic.Int32
	var group sync.WaitGroup
	receipts := make(chan Receipt, 8)
	admit := func() {
		r, owner, err := j.Reserve(t.Context(), k, i, func() (Binding, error) { allocations.Add(1); return newTestBinding(t, j, k, i, env) })
		checkConcurrentAdmission(t, r, owner, err)
		if owner {
			owners.Add(1)
		}
		if r.Binding.Reservation.Instance != "" {
			receipts <- r
		}
	}
	for range 8 {
		group.Go(admit)
	}
	group.Wait()
	close(receipts)
	if owners.Load() != 1 || allocations.Load() != 1 {
		t.Fatalf("owners=%d allocations=%d", owners.Load(), allocations.Load())
	}
	original, err := j.Lookup(t.Context(), k)
	if !errors.Is(err, ErrProgress) {
		t.Fatal(err)
	}
	for r := range receipts {
		if r.Binding != original.Binding {
			t.Fatal("equal caller returned another attempt")
		}
	}
	_, owner, err := reserveTest(t, j, k, i, env)
	if owner || !errors.Is(err, ErrProgress) {
		t.Fatalf("post-wave retry became owner: %v %v", owner, err)
	}
	changed := i
	changed.Command = []string{"/bin/false"}
	if _, owner, err := reserveTest(t, j, k, changed, env); !errors.Is(err, ErrConflict) || owner {
		t.Fatalf("conflict %v %v", owner, err)
	}
	for _, scope := range []Key{{Host: k.Host, Class: "device", Principal: "A", Opaque: k.Opaque}, {Host: k.Host, Class: "device", Principal: "B", Opaque: k.Opaque}} {
		if _, owner, err := reserveTest(t, j, scope, i, env); err != nil || !owner {
			t.Fatalf("namespace %s: %v %v", scope.Principal, owner, err)
		}
	}
}
func checkConcurrentAdmission(t *testing.T, r Receipt, owner bool, err error) {
	t.Helper()
	if errors.Is(err, context.DeadlineExceeded) {
		if owner || r.Binding.Reservation.Instance != "" {
			t.Error("timed-out lock acquisition allocated an attempt")
		}
		return
	}
	if err != nil && !errors.Is(err, ErrProgress) {
		t.Error(err)
	}
}
func TestRestartDoesNotRedispatchReservedOrDispatched(t *testing.T) {
	for _, dispatchFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "reserved", true: "dispatched"}[dispatchFirst], func(t *testing.T) { testRestartPhase(t, dispatchFirst) })
	}
}
func testRestartPhase(t *testing.T, dispatchFirst bool) {
	j, k, i, env := journalFixture(t)
	r, owner, err := reserveTest(t, j, k, i, env)
	if err != nil || !owner {
		t.Fatal(err)
	}
	if dispatchFirst {
		if err := j.Dispatch(t.Context(), r.Binding, i); err != nil {
			t.Fatal(err)
		}
	}
	other, err := Open(j.files.root.Name(), "boot", "replacement")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	next, err := other.Lookup(t.Context(), k)
	if next.Binding != r.Binding {
		t.Fatal("restart replaced binding")
	}
	if dispatchFirst {
		if !errors.Is(err, ErrUnknown) || next.RetainUntil != nil {
			t.Fatalf("dispatch uncertainty: %+v %v", next, err)
		}
		return
	}
	if err != nil || next.Result == nil || next.Result.Termination.Kind != "not_started" {
		t.Fatalf("abandoned reserve: %+v %v", next, err)
	}
}
func TestClaimIsExclusiveAndResultReleasesActiveQuota(t *testing.T) {
	j, k, i, env := journalFixture(t)
	j.maxActive = 1
	r, _, err := reserveTest(t, j, k, i, env)
	if err != nil {
		t.Fatal(err)
	}
	another := k
	another.Opaque = "another"
	if _, _, err := reserveTest(t, j, another, i, env); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if err := j.Dispatch(t.Context(), r.Binding, i); err != nil {
		t.Fatal(err)
	}
	a, err := StartAttempt(t.Context(), j.files.root.Name(), r.Binding, i, env)
	if err != nil {
		t.Fatal(err)
	}
	code := 7
	now := time.Now().UTC()
	if err := a.Finish(Result{Binding: r.Binding, Termination: Termination{Kind: "exit", Code: &code}, EndedAt: &now, Capture: "not_recorded"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := StartAttempt(t.Context(), j.files.root.Name(), r.Binding, i, env); err == nil {
		t.Fatal("duplicate worker claimed child start")
	}
	terminal, err := j.Lookup(t.Context(), k)
	if err != nil || terminal.Phase != "terminal" {
		t.Fatalf("terminal %+v %v", terminal, err)
	}
	if _, owner, err := reserveTest(t, j, another, i, env); err != nil || !owner {
		t.Fatalf("fast exit retained active slot: %v %v", owner, err)
	}
}
func TestRetirementWaitsThroughFinalPathnameWork(t *testing.T) {
	j, k, i, env := journalFixture(t)
	r, _, err := reserveTest(t, j, k, i, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Dispatch(t.Context(), r.Binding, i); err != nil {
		t.Fatal(err)
	}
	a, err := StartAttempt(t.Context(), j.files.root.Name(), r.Binding, i, env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if a != nil {
			_ = a.Close()
		}
	})
	code := 0
	if err := a.Finish(Result{Binding: r.Binding, Termination: Termination{Kind: "exit", Code: &code}, Capture: "not_recorded"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	removed := false
	retire := func() error { removed = true; return nil }
	if err := j.RetireSession(ctx, filepath.Join(j.files.root.Name(), "s"), r.Binding.Reservation.ID, retire); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("durable result fenced a still-running finalizer")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a = nil
	if err := j.RetireSession(t.Context(), filepath.Join(j.files.root.Name(), "s"), r.Binding.Reservation.ID, retire); err != nil {
		t.Fatal(err)
	}
	got, err := j.Lookup(t.Context(), k)
	if err != nil || got.Removal != "removed" || got.Result.Termination.Code == nil {
		t.Fatalf("receipt lost after removal: %+v %v", got, err)
	}
}
func TestRawQuotaIncludesDamagedRecordsButEqualLookupWorks(t *testing.T) {
	j, k, i, env := journalFixture(t)
	j.maxRecords = 2
	if _, _, err := reserveTest(t, j, k, i, env); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(j.files.root.Name(), "execution/requests/damaged"), []byte("torn"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reserveTest(t, j, k, i, env); !errors.Is(err, ErrProgress) {
		t.Fatal(err)
	}
	k.Opaque = "new"
	if _, _, err := reserveTest(t, j, k, i, env); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}
func TestRetentionDoesNotCreditWallClockJumpsOrRebootDowntime(t *testing.T) {
	j, k, i, env := journalFixture(t)
	anchor := clockEvidence("boot", 1)
	j.clock = func() ClockAnchor { return anchor }
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	j.now = func() time.Time { return now }
	r, _, err := reserveTest(t, j, k, i, env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.NotStarted(t.Context(), r.Binding, "test_no_start"); err != nil {
		t.Fatal(err)
	}
	if err := j.RetireSession(t.Context(), filepath.Join(j.files.root.Name(), "s"), r.Binding.Reservation.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	now = now.Add(90 * 24 * time.Hour)
	got, err := j.Lookup(t.Context(), k)
	if err != nil || got.RetainUntil != nil || got.RetentionReason != "elapsed_unconfirmed" {
		t.Fatalf("jump: %+v %v", got, err)
	}
	if err := j.Maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	anchor = clockEvidence("new-boot", 100)
	if _, err := j.Lookup(t.Context(), k); err != nil {
		t.Fatal(err)
	}
	if err := j.Maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	anchor = clockEvidence(anchor.Boot, anchor.Elapsed+int64(receiptFloor))
	now = now.Add(receiptFloor)
	if err := j.Maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Lookup(t.Context(), k); !errors.Is(err, ErrNotFound) {
		t.Fatalf("elapsed floor did not expire: %v", err)
	}
}

func TestCorruptClockAnchorIsReanchoredWithoutCreditingTime(t *testing.T) {
	j, k, i, env := journalFixture(t)
	evidence := clockEvidence("boot", int64(80*24*time.Hour))
	j.clock = func() ClockAnchor { return evidence }
	r, _, err := reserveTest(t, j, k, i, env)
	if err != nil {
		t.Fatal(err)
	}
	r, err = j.NotStarted(t.Context(), r.Binding, "no_child")
	if err != nil {
		t.Fatal(err)
	}
	r.Anchor.Elapsed = 0
	if _, err := j.save(r); err != nil {
		t.Fatal(err)
	}
	next, err := j.Lookup(t.Context(), k)
	if err != nil {
		t.Fatal(err)
	}
	if next.Anchor.Elapsed != evidence.Elapsed || !next.Anchor.valid() || elapsedFloor(*next.Anchor, evidence) {
		t.Fatal("corrupt anchor credited elapsed time")
	}
}

func TestMissingLifetimeInodeDoesNotFenceAnExistingWorker(t *testing.T) {
	j, k, i, env := journalFixture(t)
	r, _, err := reserveTest(t, j, k, i, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Dispatch(t.Context(), r.Binding, i); err != nil {
		t.Fatal(err)
	}
	a, err := StartAttempt(t.Context(), j.files.root.Name(), r.Binding, i, env)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	code := 0
	if err := a.Finish(Result{Binding: r.Binding, Termination: Termination{Kind: "exit", Code: &code}, Capture: "not_recorded"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(j.files.root.Name(), attempt(r.Binding, "lifetime.lock"))); err != nil {
		t.Fatal(err)
	}
	removed := false
	err = j.RetireSession(t.Context(), filepath.Join(j.files.root.Name(), "s"), r.Binding.Reservation.ID, func() error { removed = true; return nil })
	if err == nil || removed {
		t.Fatal("missing lock inode was replaced to obtain retirement")
	}
}

func TestVisibleUnconfirmedAcceptanceCannotDispatch(t *testing.T) {
	j, k, i, env := journalFixture(t)
	publications := 0
	j.files.fault = func(operation string) error {
		if operation == "publish" {
			publications++
		}
		if publications == 4 && operation == "directory_sync" {
			return errors.New("injected acceptance namespace barrier")
		}
		return nil
	}
	r, owner, err := reserveTest(t, j, k, i, env)
	if err == nil || owner || r.Acceptance != string(Unconfirmed) {
		t.Fatalf("unconfirmed reserve elected launch owner: %+v %v %v", r, owner, err)
	}
	if err := j.Dispatch(t.Context(), r.Binding, i); err == nil {
		t.Fatal("unconfirmed reserve dispatched")
	}
	if err := j.files.check(attempt(r.Binding, "dispatch.json"), false); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dispatch evidence created before acceptance")
	}
	j.files.fault = nil
	next, owner, err := reserveTest(t, j, k, i, env)
	if owner || !errors.Is(err, ErrProgress) || next.Binding != r.Binding || next.Acceptance != string(Durable) {
		t.Fatalf("settlement replaced attempt: %+v %v %v", next, owner, err)
	}
}
func TestDispatchBarrierFailureNeverElectsAnotherOwner(t *testing.T) {
	j, k, i, env := journalFixture(t)
	r, _, err := reserveTest(t, j, k, i, env)
	if err != nil {
		t.Fatal(err)
	}
	published := false
	j.files.fault = func(operation string) error {
		if operation == "publish" {
			published = true
		}
		if published && operation == "directory_sync" {
			return errors.New("injected dispatch namespace barrier")
		}
		return nil
	}
	if err := j.Dispatch(t.Context(), r.Binding, i); err == nil {
		t.Fatal("dispatch barrier acknowledged")
	}
	j.files.fault = nil
	next, owner, err := reserveTest(t, j, k, i, env)
	if owner || !errors.Is(err, ErrUnknown) || next.Binding != r.Binding || next.Phase != phaseUncertain {
		t.Fatalf("uncertain dispatch admitted replacement: %+v %v %v", next, owner, err)
	}
}
