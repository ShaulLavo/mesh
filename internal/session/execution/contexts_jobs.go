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
	return j.reserveContextJob(ctx, k, callerDigest, resolve, reserve, nil)
}

// ReserveInContextAndDispatch is ReserveInContext for a caller that launches
// the job at once, as ReserveAndDispatch is for Reserve. For a new job it
// also returns the launch snapshot it read back under the journal lock before
// dispatching, which is what the caller launches: reading it again after
// dispatch would leave a window where a dying coordinator reports an unknown
// outcome for a job no worker could have started.
func (j *Journal) ReserveInContextAndDispatch(ctx context.Context, k Key, callerDigest string, resolve ResolveContextJob, reserve func(LaunchSnapshot) (Binding, error)) (Receipt, LaunchSnapshot, bool, error) {
	var launch LaunchSnapshot
	r, owner, err := j.reserveContextJob(ctx, k, callerDigest, resolve, reserve, &launch)
	return r, launch, owner, err
}

// reserveContextJob dispatches a new job when launch is set, and stores the
// snapshot it dispatched in launch.
func (j *Journal) reserveContextJob(ctx context.Context, k Key, callerDigest string, resolve ResolveContextJob, reserve func(LaunchSnapshot) (Binding, error), launch *LaunchSnapshot) (Receipt, bool, error) {
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
	r, owner, err := j.reserveInContext(ctx, k, callerDigest, resolve, reserve, launch)
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

// ResolveRun is the acceptance point of a request-scoped run in an owner's
// context: the same ownership, closure, damage and intent checks a keyed
// context job passes, under the admitted lock, with no record of the run.
// Only the context's use is recorded, at most once per granularity, so a
// context used only this way does not look idle.
func (j *Journal) ResolveRun(ctx context.Context, owner, contextID string, resolve ResolveContextJob) (LaunchSnapshot, error) {
	gate, err := j.lockAdmitted(ctx)
	if err != nil {
		return LaunchSnapshot{}, err
	}
	defer func() { _ = gate.Close() }()
	c, snapshot, err := j.resolveOpenContext(Key{Principal: owner, Context: contextID}, resolve)
	if err != nil {
		return LaunchSnapshot{}, err
	}
	// A failed use update costs only an earlier expiry candidate, which still
	// needs a full idle period; it must not refuse the run.
	_, _ = j.touch(c, false)
	return snapshot, nil
}

func (j *Journal) resolveOpenContext(k Key, resolve ResolveContextJob) (Context, LaunchSnapshot, error) {
	c, err := j.ownedContext(k.Principal, k.Context)
	if err != nil {
		return Context{}, LaunchSnapshot{}, err
	}
	if c.ClosedAt != nil {
		return Context{}, LaunchSnapshot{}, ErrContextClosed
	}
	if err := j.requireMembers(c.ID); err != nil {
		return Context{}, LaunchSnapshot{}, err
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

func (j *Journal) reserveInContext(ctx context.Context, k Key, callerDigest string, resolve ResolveContextJob, reserve func(LaunchSnapshot) (Binding, error), launch *LaunchSnapshot) (Receipt, bool, error) {
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
		prepare: func(b Binding, r *round) error { return prepareContextJob(r, b, k, snapshot) },
	}
	r, owner, err := j.acceptNew(ctx, k, snapshot.Intent, func() (Binding, error) { return reserve(snapshot) }, hooks)
	if err != nil || !owner {
		return r, owner, err
	}
	// Membership is already durable through the marker, so a failed use
	// update cannot leave the running job's context unprotected.
	_, _ = j.touch(c, true)
	if launch == nil {
		return r, true, nil
	}
	return j.dispatchContextJob(r, launch)
}

// dispatchContextJob dispatches a context job in the lock hold that accepted
// it, after the context update and once the launch snapshot reads back as the
// receipt recorded it. Until then the receipt stays reserved, so a
// coordinator that dies first still settles as not_started. A snapshot that
// does not read back settles the job as not_started here, and the caller does
// not launch it. The snapshot read here is stored in launch for the caller.
func (j *Journal) dispatchContextJob(r Receipt, launch *LaunchSnapshot) (Receipt, bool, error) {
	snapshot, err := j.ReadLaunchSnapshot(r)
	if err != nil {
		settled, saveErr := j.terminal(r, Result{Binding: r.Binding, Termination: Termination{Kind: notStarted, Reason: "launch_snapshot_unreadable"}, Capture: CaptureNotRecorded})
		return settled, false, errors.Join(executionBoundaryError("read context launch", err), saveErr)
	}
	r, err = j.dispatchReserved(r, snapshot.Intent)
	if err == nil {
		*launch = snapshot
	}
	return r, true, err
}

// prepareContextJob adds the launch snapshot and the membership marker to the
// attempt's sync round, so both are durable before the receipt. Neither
// depends on the other or on the attempt reference.
func prepareContextJob(r *round, b Binding, k Key, snapshot LaunchSnapshot) error {
	if err := r.record(launchSnapshotName(b), snapshot, true, contextRecordLimit); err != nil {
		return err
	}
	// The open created the membership directory and resolveOpenContext saw
	// it; recreating it here would hide that it went missing.
	return r.record(contextMember(k.Context, b.RequestHash), k, true, MaxRecord)
}

// requireMembers refuses a job for a context whose membership directory
// cannot be listed, the same listing expiry relies on. Every open creates it,
// so a missing or unreadable one is damage that may hide a running job; a new
// marker in a recreated directory would make membership look complete and
// let expiry remove the context. Preflight runs it before output admission,
// so a refused job reclaims no retained output.
func (j *Journal) requireMembers(id string) error {
	if _, err := j.files.Names(contextMembers(id)); err != nil {
		return fmt.Errorf("context %s: %w", id, errors.Join(ErrContextDamaged, err))
	}
	return nil
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
