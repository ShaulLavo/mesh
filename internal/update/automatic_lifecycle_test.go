package update

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestAutomaticReviewRetiredFailedRunDoesNotBlockScheduler(t *testing.T) {
	c, remote, actor := automaticFixture(t, 2)
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	info := remote.info[remote.grants[0]]
	info.Installation.Phase = updateinstall.RolledBack
	remote.info[remote.grants[0]] = info
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	decideAutomatic(t, c, actor, "cancel")
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	old := currentAutomatic(t, c).Current.ID
	stale, err := c.Store.Read(old)
	if err != nil {
		t.Fatal(err)
	}
	decideAutomatic(t, c, actor, "acknowledge-stop")
	if err := c.stepRun(t.Context(), stale); err != nil {
		t.Fatalf("snapshot racing retirement reentered scheduler: %v", err)
	}
	history, err := c.Store.Read(old)
	if err != nil || history.Targets[0].State != Failed {
		t.Fatalf("retirement changed failure history: %+v %v", history, err)
	}
	if len(Unfinished([]Run{history})) != 0 {
		t.Fatal("status-only history still needs execution attention")
	}
	cache := t.TempDir()
	if err := os.Mkdir(filepath.Join(cache, history.ReleaseDigest), 0700); err != nil {
		t.Fatal(err)
	}
	if err := PruneCache(t.Context(), c.Store.stateDir(), cache); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, history.ReleaseDigest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired failed history pins cache: %v", err)
	}
	decideAutomatic(t, c, actor, "enable")
	if err := c.Step(t.Context()); err != nil {
		t.Fatalf("retired automatic history %s reentered scheduler: %v", old, err)
	}
}

func TestAutomaticReviewAutomaticFleetMustMatchEnrolledDigest(t *testing.T) {
	c, _, _ := automaticFixture(t, 2)
	r := currentAutomatic(t, c)
	id := r.Current.Targets[0].Host.ID
	for i := range r.Current.Fleet.Members {
		if r.Current.Fleet.Members[i].ID == id {
			r.Current.Fleet.Members[i].Endpoint = "ws://changed.invalid:7337/mesh"
		}
	}
	r.Current.Targets[0].Host.Endpoint = "ws://changed.invalid:7337/mesh"
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(c.Store.directory), "automatic.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.ReadAutomatic(t.Context()); err == nil {
		t.Fatal("accepted current fleet and target endpoint not bound to saved membership digest")
	}
}

func TestAutomaticReviewDisabledContinuationCanBeRetriedByLocalCoordinator(t *testing.T) {
	c, _, _ := automaticFixture(t, 2)
	r := currentAutomatic(t, c)
	owner, _ := testIdentity(t)
	r.Policy.OwnerID = owner
	if err := c.Store.writeAutomatic(r); err != nil {
		t.Fatal(err)
	}
	local := VerifiedActor{id: c.ID, local: true}
	decideAutomatic(t, c, local, "disable")
	decideAutomatic(t, c, local, "retry")
	r = currentAutomatic(t, c)
	_, issued, err := c.reserveAutomatic(t.Context(), r.Current.ID, 0, "stage")
	if err != nil || !issued {
		t.Fatalf("accepted local explicit continuation cannot issue: issued=%v error=%v", issued, err)
	}
}

type withheldAutomaticStage struct {
	*automaticRemote
	request *updateinstall.Request
	host    Host
}

func (r *withheldAutomaticStage) Call(ctx context.Context, host Host, action string, input, output any) error {
	if action == "stage" && r.request == nil {
		request := input.(updateinstall.Request)
		r.request = &request
		r.host = host
		return errors.New("stage acknowledgement lost before acceptance becomes visible")
	}
	return r.automaticRemote.Call(ctx, host, action, input, output)
}
func TestAutomaticReviewCancelledIssuedStageCannotRetireBeforeAcceptanceSettles(t *testing.T) {
	c, remote, actor := automaticFixture(t, 1)
	delayed := &withheldAutomaticStage{automaticRemote: remote}
	c.Remote = delayed
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if delayed.request == nil {
		t.Fatal("stage not issued")
	}
	decideAutomatic(t, c, actor, "cancel")
	// Clear only the observation backoff. It is not authority or settlement.
	r := currentAutomatic(t, c)
	if err := c.record(r.Current.ID, 0, func(target *Target) { target.RetryAt = r.Current.CreatedAt }); err != nil {
		t.Fatal(err)
	}
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	r = currentAutomatic(t, c)
	cmd := AutomaticCommand{ID: mustAutomaticID(t), Action: "acknowledge-stop", RunID: r.Current.ID, ExpectedRevision: r.Policy.Revision}
	if _, err := c.ChangeAutomatic(t.Context(), actor, cmd); err == nil {
		status := completeDelayedAutomaticStage(t, c, r, delayed)
		t.Fatalf("retired issued stage before delayed acceptance: target phase=%s", status.Phase)
	}
	reopened, err := OpenStore(c.Store.stateDir())
	if err != nil {
		t.Fatal(err)
	}
	c.Store = reopened
	completeDelayedAutomaticStage(t, c, r, delayed)
	clearAutomaticObservationBackoff(t, c)
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	settled := currentAutomatic(t, c).Current.Targets[0]
	if settled.State != Cancelled || settled.StageAttempt == 0 || settled.StageUnsettled {
		t.Fatalf("late acceptance did not settle exact issued work: %+v", settled)
	}
	decideAutomatic(t, c, actor, "acknowledge-stop")
}

func completeDelayedAutomaticStage(t *testing.T, c *Coordinator, r AutomaticRecord, delayed *withheldAutomaticStage) updateinstall.Status {
	t.Helper()
	recipient, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = recipient.SetRecipientConsent(t.Context(), 0, ConsentChange{LocalHostID: delayed.host.ID, CoordinatorID: c.ID, Fleet: r.Policy.Fleet, Epoch: r.Policy.Epoch, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := recipient.AuthorizeAcceptance(VerifiedActor{id: c.ID}, func(string) bool { return true })(t.Context(), *delayed.request, updateinstall.AcceptanceStage, func() error { return nil }); err != nil {
		t.Fatalf("late recipient acceptance unexpectedly refused: %v", err)
	}
	var status updateinstall.Status
	if err := delayed.automaticRemote.Call(t.Context(), delayed.host, "stage", *delayed.request, &status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestAutomaticReviewRetiredAutomaticHistoryDoesNotPinArtifactsForever(t *testing.T) {
	c, _, _ := automaticFixture(t, 1)
	r := currentAutomatic(t, c)
	old := r.Current.ReleaseDigest
	if _, err := c.Store.Change(r.Current.ID, func(run *Run) error { run.Targets[0].State = Updated; return nil }); err != nil {
		t.Fatal(err)
	}
	checked, err := c.CheckedRelease(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checked.Manifest.Version = "v0.3.0"
	checked.Digest = checked.Manifest.Digest()
	if _, err := c.Store.AcceptCheckedRelease(t.Context(), *checked, c.automaticScope()); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	if err := os.Mkdir(filepath.Join(cache, old), 0700); err != nil {
		t.Fatal(err)
	}
	if err := PruneCache(t.Context(), c.Store.stateDir(), cache); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, old)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal status-only automatic history still pins obsolete artifact directory: %v", err)
	}
}

func TestAutomaticReviewRunConsumesCheckedReleaseWithoutFixtureStep(t *testing.T) {
	c, remote, _ := automaticFixture(t, 1)
	r := currentAutomatic(t, c)
	r.Current = nil
	r.Attempt = 0
	r.Enrollment = nil
	if err := c.Store.writeAutomatic(r); err != nil {
		t.Fatal(err)
	}
	c.lastAutomaticCheck = time.Time{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, func(err error) { t.Errorf("scheduler: %v", err); cancel() }) }()
	t.Cleanup(func() { cancel(); <-done })
	// Observe the persisted grant, never inspect the remote fixture concurrently.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		status, err := c.Store.ReadAutomatic(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if status.Record.Current != nil && status.Record.Current.Targets[0].Grant {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("joined scheduler never issued its authorized canary")
		case <-poll.C:
		}
	}
	cancel()
	<-done
	if len(remote.grants) != 1 {
		t.Fatalf("scheduler issued %d grants, want one", len(remote.grants))
	}
}

// A consumed retry token fences retransmission; the older terminal journal does not.
type withheldAutomaticRetry struct {
	*automaticRemote
	operation *Operation
	host      Host
}

func (r *withheldAutomaticRetry) Call(ctx context.Context, host Host, action string, input, output any) error {
	if action == "install-retry" && r.operation == nil {
		operation := input.(Operation)
		r.operation, r.host = &operation, host
		return errors.New("retry acknowledgement lost before acceptance becomes visible")
	}
	return r.automaticRemote.Call(ctx, host, action, input, output)
}

func TestAutomaticReviewCancelledIssuedRetryCannotRetireOnOldTerminalJournal(t *testing.T) {
	c, remote, actor := automaticFixture(t, 1)
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	r := currentAutomatic(t, c)
	host := r.Current.Targets[0].Host.ID
	info := remote.info[host]
	info.Installation.Phase = updateinstall.RolledBack
	remote.info[host] = info
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	decideAutomatic(t, c, actor, "retry")
	delayed := &withheldAutomaticRetry{automaticRemote: remote}
	c.Remote = delayed
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if delayed.operation == nil {
		t.Fatal("retry was not issued")
	}
	decideAutomatic(t, c, actor, "cancel")
	clearAutomaticObservationBackoff(t, c)
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	r = currentAutomatic(t, c)
	cmd := AutomaticCommand{ID: mustAutomaticID(t), Action: "acknowledge-stop", RunID: r.Current.ID, ExpectedRevision: r.Policy.Revision}
	if _, err := c.ChangeAutomatic(t.Context(), actor, cmd); err == nil {
		var status updateinstall.Status
		if err := remote.Call(t.Context(), delayed.host, "install-retry", *delayed.operation, &status); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("retired issued retry before its token settled: phase=%s token=%d", status.Phase, status.RetryToken)
	}
	var status updateinstall.Status
	if err := remote.Call(t.Context(), delayed.host, "install-retry", *delayed.operation, &status); err != nil {
		t.Fatal(err)
	}
	clearAutomaticObservationBackoff(t, c)
	if err := c.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	decideAutomatic(t, c, actor, "acknowledge-stop")
}

func clearAutomaticObservationBackoff(t *testing.T, c *Coordinator) {
	t.Helper()
	r := currentAutomatic(t, c)
	if err := c.record(r.Current.ID, 0, func(target *Target) { target.RetryAt = time.Time{} }); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticReviewRetirementAfterSchedulerReadDoesNotWriteHistory(t *testing.T) {
	c, remote, _ := automaticFixture(t, 1)
	snapshot := *currentAutomatic(t, c).Current
	retired := false
	c.Download = func(ctx context.Context, _ release.Manifest, _ release.Platform, _ string) (string, error) {
		if retired {
			return "/cached/mesh", nil
		}
		retired = true
		if _, err := c.Store.Change(snapshot.ID, func(run *Run) error { run.Targets[0].State = Updated; return nil }); err != nil {
			return "", err
		}
		checked, err := c.CheckedRelease(ctx)
		if err != nil {
			return "", err
		}
		checked.Manifest.Version = "v0.3.0"
		checked.Digest = checked.Manifest.Digest()
		_, err = c.Store.AcceptCheckedRelease(ctx, *checked, c.automaticScope())
		return "/cached/mesh", err
	}
	if err := c.stepRun(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	if !retired || remote.stages != 0 {
		t.Fatal("stale scheduler dispatched retired history")
	}
	history, err := c.Store.Read(snapshot.ID)
	if err != nil || history.Cached || history.Targets[0].State != Updated {
		t.Fatalf("stale scheduler changed history: %+v %v", history, err)
	}
}

func TestAutomaticReviewReceiptSettlementCannotReusePriorFailedState(t *testing.T) {
	c, _, actor := automaticFixture(t, 1)
	r := currentAutomatic(t, c)
	if _, err := c.Store.Change(r.Current.ID, func(run *Run) error { run.Targets[0].Generation = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	run, issued, err := c.reserveAutomatic(t.Context(), r.Current.ID, 0, "stage")
	if err != nil || !issued {
		t.Fatalf("reserve stage: %v %v", issued, err)
	}
	if err := c.fail(run.ID, 0, errors.New("response lost")); err != nil {
		t.Fatal(err)
	}
	decideAutomatic(t, c, actor, "cancel")
	run = *currentAutomatic(t, c).Current
	target := run.Targets[0]
	status := updateinstall.Status{Phase: updateinstall.Accepted, Request: updateinstall.Request{ID: run.ID, TargetID: target.Host.ID, Generation: target.Generation, Manifest: run.Release, Automatic: automaticRequest(run, target)}}
	if _, err := c.observeAutomaticDispatch(run, 0, status); err != nil {
		t.Fatal(err)
	}
	r = currentAutomatic(t, c)
	if r.Current.Targets[0].StageUnsettled || automaticSettled(*r.Current) {
		t.Fatal("nonterminal receipt reused prior Failed settlement")
	}
	cmd := AutomaticCommand{ID: mustAutomaticID(t), Action: "acknowledge-stop", RunID: run.ID, ExpectedRevision: r.Policy.Revision}
	if _, err := c.ChangeAutomatic(t.Context(), actor, cmd); err == nil {
		t.Fatal("acknowledged a nonterminal accepted stage")
	}
}

func TestAutomaticReviewInvalidReceiptRetainsIssuedIntention(t *testing.T) {
	for _, field := range []string{"id", "target", "generation", "manifest", "origin"} {
		t.Run(field, func(t *testing.T) { checkAutomaticInvalidReceipt(t, field) })
	}
}

func checkAutomaticInvalidReceipt(t *testing.T, field string) {
	t.Helper()
	c, _, actor := automaticFixture(t, 1)
	run := *currentAutomatic(t, c).Current
	if _, err := c.Store.Change(run.ID, func(run *Run) error { run.Targets[0].Generation = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	run, _, err := c.reserveAutomatic(t.Context(), run.ID, 0, "stage")
	if err != nil {
		t.Fatal(err)
	}
	decideAutomatic(t, c, actor, "cancel")
	target := run.Targets[0]
	status := updateinstall.Status{Phase: updateinstall.Cancelled, Request: updateinstall.Request{ID: run.ID, TargetID: target.Host.ID, Generation: target.Generation, Manifest: run.Release, Automatic: automaticRequest(run, target)}}
	switch field {
	case "id":
		status.Request.ID = mustAutomaticID(t)
	case "target":
		status.Request.TargetID = "wrong"
	case "generation":
		status.Request.Generation++
	case "manifest":
		status.Request.Manifest.Version = "v0.3.0"
	case "origin":
		status.Request.Automatic.Epoch++
	}
	if _, err := c.observeAutomaticDispatch(run, 0, status); err == nil {
		t.Fatal("accepted mismatched receipt")
	}
	if !currentAutomatic(t, c).Current.Targets[0].StageUnsettled {
		t.Fatal("invalid receipt erased issued intention")
	}
}
