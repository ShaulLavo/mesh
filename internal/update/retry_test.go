package update

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
)

type retryRemote struct {
	*fakeRemote
	tokens  []uint64
	loseAck bool
}

func (remote *retryRemote) Call(ctx context.Context, host Host, action string, input, output any) error {
	if action != "install-retry" {
		return remote.fakeRemote.Call(ctx, host, action, input, output)
	}
	if remote.offline[host.ID] {
		return errors.New("unreachable")
	}
	operation := input.(Operation)
	remote.tokens = append(remote.tokens, operation.RetryToken)
	info := remote.info[host.ID]
	if info.Installation.RetryToken < operation.RetryToken {
		info.Installation.RetryToken = operation.RetryToken
		info.Installation.Phase = updateinstall.Staged
		remote.info[host.ID] = info
	}
	if remote.loseAck {
		remote.loseAck = false
		return errors.New("lost retry acknowledgement")
	}
	data, err := json.Marshal(info.Installation)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, output)
}

func failedRetryFixture(t *testing.T) (*Coordinator, *retryRemote, Run) {
	t.Helper()
	c, remote, run := testCoordinator(t, 1)
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	info := remote.info[run.Targets[0].Host.ID]
	info.Installation.Phase = updateinstall.RolledBack
	info.Installation.Error = "candidate failed"
	remote.info[run.Targets[0].Host.ID] = info
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, err := c.Store.Read(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Stopped || run.Targets[0].State != Failed {
		t.Fatal("fixture did not fail")
	}
	retries := &retryRemote{fakeRemote: remote}
	c.Remote = retries
	return c, retries, run
}

func resumeRetryStep(t *testing.T, c *Coordinator, id string) Run {
	t.Helper()
	if _, err := c.Store.Change(id, func(run *Run) error { run.Targets[0].RetryAt = time.Time{}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, err := c.Store.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestExplicitRetrySurvivesOfflineTargetAndCoordinatorRestart(t *testing.T) {
	c, remote, run := failedRetryFixture(t)
	host := run.Targets[0].Host.ID
	remote.offline[host] = true
	approved, err := c.Retry(context.Background(), run.ID)
	if err != nil || !approved.Targets[0].RetryPending || approved.Targets[0].RetryToken != 1 {
		t.Fatalf("retry was not persisted offline: %+v %v", approved, err)
	}
	resumeRetryStep(t, c, run.ID)
	restarted := &Coordinator{ID: c.ID, Store: c.Store, Remote: remote, CacheDir: c.CacheDir, Download: c.Download}
	remote.offline[host] = false
	resumeRetryStep(t, restarted, run.ID)
	resumed := resumeRetryStep(t, restarted, run.ID)
	if len(remote.tokens) != 1 || remote.tokens[0] != 1 || resumed.Targets[0].State != Granted || resumed.Stopped {
		t.Fatalf("offline retry did not resume: %+v tokens=%v", resumed, remote.tokens)
	}
}

func TestRetryLostAcknowledgementDoesNotAllocateAnotherRetry(t *testing.T) {
	c, remote, run := failedRetryFixture(t)
	if _, err := c.Retry(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	remote.loseAck = true
	pending := resumeRetryStep(t, c, run.ID)
	if !pending.Targets[0].RetryPending {
		t.Fatal("lost acknowledgement erased retry intent")
	}
	replayed, err := c.Retry(context.Background(), run.ID)
	if err != nil || replayed.Targets[0].RetryToken != 1 {
		t.Fatalf("replayed approval allocated another token: %+v %v", replayed, err)
	}
	resumed := resumeRetryStep(t, c, run.ID)
	if resumed.Targets[0].State != Granted || resumed.Targets[0].RetryPending || len(remote.tokens) != 1 {
		t.Fatalf("did not reconcile consumed retry: %+v tokens=%v", resumed, remote.tokens)
	}
}

func TestCancelledOfflineRetryDoesNotDispatchInstallationRetry(t *testing.T) {
	c, remote, run := failedRetryFixture(t)
	if _, err := c.Retry(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.Cancel(run.ID); err != nil {
		t.Fatal(err)
	}
	resumeRetryStep(t, c, run.ID)
	if len(remote.tokens) != 0 {
		t.Fatal("cancelled retry was sent after reconnect")
	}
}

func TestReplayedRetryAfterSecondRollbackDoesNotReactivate(t *testing.T) {
	c, remote, run := failedRetryFixture(t)
	if _, err := c.Retry(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	remote.loseAck = true
	resumeRetryStep(t, c, run.ID)
	info := remote.info[run.Targets[0].Host.ID]
	info.Installation.Phase = updateinstall.RolledBack
	remote.info[run.Targets[0].Host.ID] = info
	resumed := resumeRetryStep(t, c, run.ID)
	if !resumed.Stopped || resumed.Targets[0].State != Failed || resumed.Targets[0].RetryPending || len(remote.tokens) != 1 {
		t.Fatalf("same retry token reactivated after failure: %+v tokens=%v", resumed, remote.tokens)
	}
}
