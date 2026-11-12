package update

import (
	"testing"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
)

func TestHostTargetFailedExplicitRetryIsQuarantinedAgain(t *testing.T) {
	t.Run("restart after rollback", func(t *testing.T) { testFailedExplicitRetry(t, false) })
	t.Run("restart after intent settlement", func(t *testing.T) { testFailedExplicitRetry(t, true) })
}

func testFailedExplicitRetry(t *testing.T, settled bool) {
	t.Helper()
	f := newHostFixture(t)
	f.head(2)
	f.tick()
	f.fail = "v0.1.2"
	if status, err := f.engine.Run(t.Context()); err == nil || status.Phase != updateinstall.RolledBack {
		t.Fatalf("initial rollback: %s %v", status.Phase, err)
	}
	f.tick()
	input := localTargetInput(t, f, "v0.1.2")
	input.RetryEdge = &Edge{From: f.current.Digest, To: f.builds["v0.1.2"].Digest, Platform: f.current.Platform}
	reserved, err := f.updater.SubmitLocalTarget(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := f.updater.loadTarget(t.Context())
	if err != nil || intent == nil || intent.Work == nil {
		t.Fatalf("reserved retry binding: %#v, %v", intent, err)
	}
	workDigest := targetWorkDigest(*intent.Work)
	f.updater = f.newUpdater()
	f.tick()
	if status := f.installation(); status.Request.ID != reserved.Request || status.Request.Generation != reserved.Generation {
		t.Fatal("restart replaced the approved pre-journal retry reservation")
	}
	if status, err := f.engine.Run(t.Context()); err == nil || status.Phase != updateinstall.RolledBack {
		t.Fatalf("explicit retry rollback: %s %v", status.Phase, err)
	}
	if settled {
		settleRetryBeforeOwnershipClear(t, f)
	}
	f.updater = f.newUpdater()
	if err := f.updater.Tick(t.Context()); err == nil {
		t.Fatal("settled retry was not held by quarantine")
	}
	status := f.installation()
	if status.Phase != updateinstall.RolledBack || status.Request.ID != reserved.Request {
		t.Fatalf("one explicit retry authorized more work after another rollback: phase %s, generation %d, same request %v", status.Phase, status.Request.Generation, status.Request.ID == reserved.Request)
	}
	intent, err = f.updater.loadTarget(t.Context())
	if err != nil || intent == nil || intent.RetrySettled != workDigest || !targetSubmissionSame(intent, input) {
		t.Fatalf("retry settlement changed submission or lost consumed work: %#v, %v", intent, err)
	}
	replayed, err := f.updater.SubmitLocalTarget(t.Context(), input)
	if err != nil || replayed.Request != reserved.Request || replayed.Generation != reserved.Generation {
		t.Fatalf("retry replay replaced the settled reservation: %#v, %v", replayed, err)
	}
	f.now = f.now.Add(time.Hour)
	f.updater = f.newUpdater()
	if err := f.updater.Tick(t.Context()); err == nil {
		t.Fatal("consumed retry was not held after backoff and restart")
	}
	if status := f.installation(); status.Phase != updateinstall.RolledBack || status.Request.ID != reserved.Request {
		t.Fatal("consumed retry resumed after backoff and restart")
	}
	cancelled, err := f.store.CancelLocalTarget(t.Context(), replayed.Revision)
	if err != nil {
		t.Fatal(err)
	}
	input.ExpectedRevision = cancelled.Revision
	f.fail = ""
	approved, err := f.updater.SubmitLocalTarget(t.Context(), input)
	if err != nil || approved.Request == reserved.Request || approved.Generation != reserved.Generation+1 {
		t.Fatalf("fresh explicit retry did not reserve new work: %#v, %v", approved, err)
	}
	f.tick()
	f.finish()
	f.tick()
	if status, err := f.store.HostTarget(t.Context()); err != nil || status.Phase != HostTargetCommitted {
		t.Fatalf("fresh explicit retry failed to finish: %#v, %v", status, err)
	}
}

func settleRetryBeforeOwnershipClear(t *testing.T, f *hostFixture) {
	t.Helper()
	state, err := f.store.HostState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	journal := f.installation()
	unlock, err := f.store.lockContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := f.updater.recordFinalEvidence(state.Owned, journal); err != nil {
		t.Fatal(err)
	}
}
