package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// LaunchSnapshot is what a context job was reserved to run: the resolved
// intent and environment overlay at one context revision. It is stored in the
// job's private attempt directory before the receipt becomes durable, so a
// crash and a later context edit cannot change what the receipt accepted.
type LaunchSnapshot struct {
	Context ContextUse        `json:"context"`
	Intent  Intent            `json:"intent"`
	Env     map[string]string `json:"env,omitempty"`
}

// ResolveContextJob turns a context revision into a launch intent and
// environment overlay. It must not have effects; it may run more than once.
type ResolveContextJob func(Context) (Intent, map[string]string, error)

func launchSnapshotName(b Binding) string { return attempt(b, "launch.json") }

// ReserveInContext reserves a job against the current revision of an open
// context. Ownership, closure and the resolved intent are checked without
// effects before output admission can reclaim anything, and checked again
// under the lock that accepts the job. An existing receipt for the key is
// returned before the context is read, so replays survive later edits and
// closure. callerDigest covers the caller-supplied request.
func (j *Journal) ReserveInContext(ctx context.Context, k Key, callerDigest string, resolve ResolveContextJob, reserve func(LaunchSnapshot) (Binding, error)) (Receipt, bool, error) {
	return j.reserveContextJob(ctx, k, callerDigest, resolve, reserve, false)
}

// ReserveInContextAndDispatch is ReserveInContext for a caller that launches
// the job at once, as ReserveAndDispatch is for Reserve.
func (j *Journal) ReserveInContextAndDispatch(ctx context.Context, k Key, callerDigest string, resolve ResolveContextJob, reserve func(LaunchSnapshot) (Binding, error)) (Receipt, bool, error) {
	return j.reserveContextJob(ctx, k, callerDigest, resolve, reserve, true)
}

func (j *Journal) reserveContextJob(ctx context.Context, k Key, callerDigest string, resolve ResolveContextJob, reserve func(LaunchSnapshot) (Binding, error), dispatch bool) (Receipt, bool, error) {
	if err := k.Validate(); err != nil || k.Operation != OperationExec || !validDigest(callerDigest) {
		return Receipt{}, false, errors.Join(fmt.Errorf("execution: invalid context job key"), err)
	}
	existing, found, err := j.preflightContextJob(ctx, k, callerDigest, resolve)
	if err != nil {
		return Receipt{}, false, err
	}
	if found {
		return j.replayContextJob(ctx, existing)
	}
	if err := j.prepareOutputAdmission(ctx, k); err != nil {
		return Receipt{}, false, err
	}
	r, owner, err := j.reserveInContext(ctx, k, callerDigest, resolve, reserve, dispatch)
	if err != nil || owner {
		return r, owner, err
	}
	return j.replayContextJob(ctx, r)
}

func (j *Journal) replayContextJob(ctx context.Context, r Receipt) (Receipt, bool, error) {
	r, err := j.refresh(ctx, r)
	if err == nil {
		err = receiptDiagnostic(r)
	}
	return r, false, err
}

// preflightContextJob authorizes a reservation without writing anything
// another caller could cause.
func (j *Journal) preflightContextJob(ctx context.Context, k Key, callerDigest string, resolve ResolveContextJob) (Receipt, bool, error) {
	gate, err := j.lockAdmitted(ctx)
	if err != nil {
		return Receipt{}, false, err
	}
	defer func() { _ = gate.Close() }()
	r, found, err := j.existingContextJob(k, callerDigest)
	if err != nil || found {
		return r, found, err
	}
	_, _, err = j.resolveOpenContext(k, resolve)
	return Receipt{}, false, err
}

// existingContextJob reads the receipt for k. Its key already names this
// caller, so reading it has no cross-caller effect.
func (j *Journal) existingContextJob(k Key, callerDigest string) (Receipt, bool, error) {
	r, err := j.read(k.Hash())
	if errors.Is(err, os.ErrNotExist) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if r.Key != k || r.IntentDigest != callerDigest {
		return Receipt{}, false, ErrConflict
	}
	return r, true, nil
}

func (j *Journal) resolveOpenContext(k Key, resolve ResolveContextJob) (Context, LaunchSnapshot, error) {
	c, err := j.ownedContext(k.Principal, k.Context)
	if err != nil {
		return Context{}, LaunchSnapshot{}, err
	}
	if c.ClosedAt != nil {
		return Context{}, LaunchSnapshot{}, ErrContextClosed
	}
	intent, env, err := resolve(c)
	if err != nil {
		return Context{}, LaunchSnapshot{}, err
	}
	normalized, err := Normalize(intent)
	if err != nil {
		return Context{}, LaunchSnapshot{}, err
	}
	if len(env) == 0 {
		env = nil
	}
	return c, LaunchSnapshot{Context: ContextUse{ID: c.ID, Revision: c.Revision}, Intent: normalized, Env: env}, nil
}

func (j *Journal) reserveInContext(ctx context.Context, k Key, callerDigest string, resolve ResolveContextJob, reserve func(LaunchSnapshot) (Binding, error), dispatch bool) (Receipt, bool, error) {
	gate, err := j.lockAdmitted(ctx)
	if err != nil {
		return Receipt{}, false, err
	}
	defer func() { _ = gate.Close() }()
	r, found, err := j.existingContextJob(k, callerDigest)
	if err != nil || found {
		return r, false, err
	}
	c, snapshot, err := j.resolveOpenContext(k, resolve)
	if err != nil {
		return Receipt{}, false, err
	}
	hooks := acceptHooks{
		decorate: func(r *Receipt) {
			use := snapshot.Context
			r.IntentDigest, r.LaunchIntentDigest, r.Context = callerDigest, digest(snapshot.Intent), &use
		},
		prepare:  func(b Binding, r *round) error { return prepareContextJob(r, b, k, snapshot) },
		dispatch: dispatch,
	}
	r, owner, err := j.acceptNew(ctx, k, snapshot.Intent, func() (Binding, error) { return reserve(snapshot) }, hooks)
	if err != nil || !owner {
		return r, owner, err
	}
	// Membership is already durable through the marker, so a failed use
	// update cannot leave the running job's context unprotected.
	_, _ = j.touch(c, true)
	return r, true, nil
}

// prepareContextJob adds the launch snapshot and the membership marker to the
// attempt's sync round, so both are durable before the receipt. Neither
// depends on the other or on the attempt reference.
func prepareContextJob(r *round, b Binding, k Key, snapshot LaunchSnapshot) error {
	if err := r.record(launchSnapshotName(b), snapshot, true, contextRecordLimit); err != nil {
		return err
	}
	if err := r.ensure(contextMembers(k.Context)); err != nil {
		return err
	}
	return r.record(contextMember(k.Context, b.RequestHash), k, true, MaxRecord)
}

// ReadLaunchSnapshot returns the snapshot a context job was reserved with.
// The launcher starts the job from it, so what runs is what was persisted.
func (j *Journal) ReadLaunchSnapshot(r Receipt) (LaunchSnapshot, error) {
	var snapshot LaunchSnapshot
	if err := j.files.ReadBounded(launchSnapshotName(r.Binding), &snapshot, contextRecordLimit); err != nil {
		return LaunchSnapshot{}, err
	}
	if r.Context == nil || snapshot.Context != *r.Context || digest(snapshot.Intent) != r.LaunchIntentDigest {
		return LaunchSnapshot{}, fmt.Errorf("execution: launch snapshot for %s does not match its receipt", r.Binding.Reservation.ID)
	}
	return snapshot, nil
}
