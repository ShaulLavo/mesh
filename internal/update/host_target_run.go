package update

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
)

func (s *Store) authorizeLocalTarget(owned OwnedRequest) error {
	intent, err := s.hostTarget()
	if err != nil {
		return err
	}
	if intent == nil {
		return errors.New("update: local target authority is missing")
	}
	return intent.matches(owned)
}

func (s *Store) localTargetWork(owned OwnedRequest) (*hostTargetIntent, error) {
	intent, err := s.hostTarget()
	if err != nil {
		return nil, err
	}
	if intent == nil {
		return nil, errors.New("update: issued local work lost its durable intent binding")
	}
	if intent.Work == nil && intent.SettledWork == targetWorkDigest(owned) {
		return intent, nil
	}
	if intent.Work == nil || !reflect.DeepEqual(*intent.Work, owned) {
		return nil, errors.New("update: issued local work lost its durable intent binding")
	}
	return intent, nil
}

func targetWorkDigest(owned OwnedRequest) string {
	data, _ := json.Marshal(owned)
	return discoveryDigest(data)
}

func (s *Store) finishLocalTarget(owned OwnedRequest, journal updateinstall.Status) error {
	intent, err := s.localTargetWork(owned)
	if err != nil {
		return err
	}
	if intent.Work == nil {
		return nil
	}
	if journal.Phase == updateinstall.Committed {
		intent.Status.RunningDigest = owned.Edge.To
	}
	if targetActive(intent.Status.Phase) {
		intent.Status.Phase = HostTargetWaiting
		if journal.Phase == updateinstall.Committed && owned.Metadata == intent.Status.Target.Metadata && owned.Edge.To == intent.Status.Target.Binary {
			intent.Status.Phase = HostTargetCommitted
		}
	}
	intent.SettledWork = targetWorkDigest(owned)
	if intent.Submission.RetryEdge != nil && *intent.Submission.RetryEdge == owned.Edge {
		intent.RetrySettled = intent.SettledWork
	}
	intent.Work, intent.Status.Next, intent.Status.Problem = nil, nil, nil
	if journal.Phase == updateinstall.RolledBack {
		intent.Status.Problem = &HostProblem{Kind: "quarantined", Detail: "Failed activation is quarantined; waiting for a verified alternative"}
	}
	return s.writeAuthority("host-target.json", intent)
}

func (u *HostUpdater) targetResumeAllowed(policy HostPolicy, owned OwnedRequest) bool {
	if owned.IntentRevision == 0 {
		return policy.Mode == HostAutomatic
	}
	return u.localTarget != nil && u.localTarget.matches(owned) == nil
}

func (u *HostUpdater) tickTarget(ctx context.Context, state HostUpdate, policy HostPolicy, intent *hostTargetIntent, journal updateinstall.Status, exists bool) error {
	if policy.Revision != intent.Status.PolicyRevision {
		_, err := u.Store.CancelLocalTarget(ctx, intent.Status.Revision)
		return err
	}
	if intent.Work != nil {
		return u.recoverTargetReservation(ctx, state, intent, journal, exists)
	}
	if exists && !hostFinished(journal.Phase) {
		return u.wait(ctx, state, "another installation is unfinished")
	}
	if err := u.targetAuthority(ctx); err != nil {
		return u.wait(ctx, state, err.Error())
	}
	if u.now().Before(state.NextAttempt) {
		return nil
	}
	input := intent.Submission
	input.CurrentDigest = intent.Status.RunningDigest
	if input.RetryEdge != nil && (input.RetryEdge.From != input.CurrentDigest || intent.RetrySettled != "") {
		input.RetryEdge = nil
	}
	prepared, err := u.planTarget(ctx, input, policy, &intent.Status.Target, false)
	if err != nil {
		return u.backoff(ctx, state, err)
	}
	if err := u.Helper(ctx, prepared.proof.Source); err != nil {
		return u.backoff(ctx, state, err)
	}
	nextIntent := *intent
	nextIntent.Status.Phase, nextIntent.Status.Next = HostTargetReserved, prepared.plan.Next
	if _, err := u.reserveTarget(ctx, policy, intent, &nextIntent, prepared); err != nil {
		return err
	}
	u.localTarget = &nextIntent
	state, err = u.Store.HostState(ctx)
	if err != nil {
		return err
	}
	return u.resume(ctx, state, policy, journal, exists, &prepared.proof)
}

func (u *HostUpdater) recoverTargetReservation(ctx context.Context, state HostUpdate, intent *hostTargetIntent, journal updateinstall.Status, exists bool) error {
	if exists && journal.Request.ID == intent.Work.Request && journal.Request.Generation == intent.Work.Generation {
		return u.attention(ctx, state, "ownership", errors.New("accepted local target has missing ownership"))
	}
	if exists && !hostFinished(journal.Phase) {
		return u.wait(ctx, state, "another installation is unfinished")
	}
	if exists && journal.Request.Generation >= intent.Work.Generation {
		return u.attention(ctx, state, "ownership", errors.New("local target reservation was superseded"))
	}
	unlock, err := u.Store.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := u.Store.hostTarget()
	if err != nil {
		return err
	}
	policy, err := u.Store.hostPolicy()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, intent) || policy.Revision != intent.Status.PolicyRevision || u.Current().Digest != intent.Work.Edge.From {
		return errors.New("update: local target reservation changed before recovery")
	}
	state.Owned, state.Phase, state.Problem, state.NextAttempt = intent.Work, HostInstalling, nil, time.Time{}
	return u.Store.writeAuthority("host-update.json", state)
}

func (u *HostUpdater) retireLocalTarget(ctx context.Context, state HostUpdate, owned OwnedRequest) error {
	if owned.IntentRevision == 0 {
		return u.save(ctx, state)
	}
	unlock, err := u.Store.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	intent, err := u.Store.localTargetWork(owned)
	if err != nil {
		return err
	}
	if targetActive(intent.Status.Phase) {
		intent.Status.Revision++
		intent.Status.Phase = HostTargetCancelled
	}
	intent.SettledWork = targetWorkDigest(owned)
	intent.Work, intent.Status.Next = nil, nil
	if err := u.Store.writeAuthority("host-target.json", intent); err != nil {
		return err
	}
	return u.Store.writeAuthority("host-update.json", state)
}
