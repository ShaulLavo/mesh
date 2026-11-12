package update

import (
	"context"
	"errors"
	"math"
	"os"
	"reflect"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

type hostTargetIntent struct {
	Schema      int                  `json:"schema"`
	Submission  HostTargetSubmission `json:"submission"`
	Status      HostTargetStatus     `json:"status"`
	Work        *OwnedRequest        `json:"work,omitempty"`
	SettledWork string               `json:"settledWork,omitempty"`
}

func targetActive(phase HostTargetPhase) bool {
	return phase == HostTargetReserved || phase == HostTargetInstalling || phase == HostTargetWaiting
}

func (s *Store) hostTarget() (*hostTargetIntent, error) {
	var intent hostTargetIntent
	err := s.readAuthority("host-target.json", &intent)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := intent.validate(); err != nil {
		return nil, err
	}
	return &intent, nil
}

func (i hostTargetIntent) validate() error {
	if i.Schema != 1 || i.Status.Schema != 1 || i.Status.Revision == 0 || i.Status.PolicyRevision != i.Submission.PolicyRevision || i.Status.CurrentDigest != i.Submission.CurrentDigest || i.Status.Target.Version != i.Submission.Version || !hostDigest(i.Status.RunningDigest) || i.Status.PendingSince.IsZero() {
		return errors.New("update: malformed local target intent")
	}
	if err := validateTargetSubmission(i.Submission); err != nil {
		return err
	}
	if err := validateTargetRelease(i.Status.Target); err != nil {
		return err
	}
	if i.SettledWork != "" && !hostDigest(i.SettledWork) {
		return errors.New("update: invalid local target settlement")
	}
	if !targetActive(i.Status.Phase) && i.Status.Phase != HostTargetCommitted && i.Status.Phase != HostTargetCancelled && i.Status.Phase != HostTargetAttention {
		return errors.New("update: invalid local target phase")
	}
	if i.Work != nil && targetActive(i.Status.Phase) {
		return i.matches(*i.Work)
	}
	return nil
}

func validateTargetSubmission(input HostTargetSubmission) error {
	return validateTargetInput(input, false)
}

func validateTargetInput(input HostTargetSubmission, preview bool) error {
	if !hostDigest(input.CurrentDigest) || input.ExpectedRevision == math.MaxUint64 {
		return errors.New("update: local target requires current executable digest and available revision")
	}
	if _, err := release.CompareVersions(input.Version, input.Version); err != nil && !(preview && input.Version == "latest") {
		return hostError(err)
	}
	if input.RetryEdge != nil && (input.RetryEdge.From != input.CurrentDigest || !hostDigest(input.RetryEdge.To) || input.RetryEdge.Platform != release.CurrentPlatform()) {
		return errors.New("update: retry approval must name an exact current-platform failed edge")
	}
	return nil
}

func validateTargetRelease(target HostTargetRelease) error {
	if !hostDigest(target.Metadata) || !hostDigest(target.Manifest) || !hostDigest(target.Binary) {
		return errors.New("update: local target lacks exact signed evidence pins")
	}
	if _, err := release.CompareVersions(target.Version, target.Version); err != nil {
		return hostError(err)
	}
	return nil
}

func (i hostTargetIntent) matches(owned OwnedRequest) error {
	if !targetActive(i.Status.Phase) || i.Status.Revision != owned.IntentRevision || i.Status.PolicyRevision != owned.PolicyRevision || i.Work == nil || !reflect.DeepEqual(*i.Work, owned) || i.Status.RunningDigest != owned.Edge.From || i.Status.Request != owned.Request || i.Status.Generation != owned.Generation || i.Status.Next == nil || i.Status.Next.Metadata != owned.Metadata || i.Status.Next.Binary != owned.Edge.To {
		return errors.New("update: local target ownership was withdrawn or changed")
	}
	if order, err := release.CompareVersions(owned.Installation.Manifest.Version, i.Status.Target.Version); err != nil || order > 0 {
		return errors.New("update: owned hop exceeds the explicitly approved target")
	}
	return owned.matches(owned.Installation)
}

func (s *Store) HostTarget(ctx context.Context) (HostTargetStatus, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return HostTargetStatus{}, err
	}
	defer unlock()
	intent, err := s.hostTarget()
	if err != nil {
		return HostTargetStatus{}, err
	}
	if intent == nil {
		return HostTargetStatus{Schema: 1, Phase: HostTargetIdle}, nil
	}
	return intent.Status, nil
}

// CancelLocalTarget fences later acceptance without waiting for the scheduler's
// network/download work. Its caller must authenticate the real local owner.
func (s *Store) CancelLocalTarget(ctx context.Context, expectedRevision uint64) (HostTargetStatus, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return HostTargetStatus{}, err
	}
	defer unlock()
	intent, err := s.hostTarget()
	if err != nil {
		return HostTargetStatus{}, err
	}
	if intent == nil || intent.Status.Revision != expectedRevision || expectedRevision == math.MaxUint64 {
		return HostTargetStatus{}, errors.New("update: local target revision changed")
	}
	intent.Status.Revision++
	intent.Status.Phase = HostTargetCancelled
	intent.Status.Problem = &HostProblem{Kind: string(HostTargetCancelled), Detail: "Local target cancelled; work already granted may finish"}
	return intent.Status, s.writeAuthority("host-target.json", intent)
}

func (u *HostUpdater) loadTarget(ctx context.Context) (*hostTargetIntent, error) {
	unlock, err := u.Store.lockContext(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return u.Store.hostTarget()
}

func (u *HostUpdater) validateTargetSnapshot(ctx context.Context, input HostTargetSubmission, preview bool) (HostPolicy, *hostTargetIntent, error) {
	if err := validateTargetInput(input, preview); err != nil {
		return HostPolicy{}, nil, err
	}
	unlock, err := u.Store.lockContext(ctx)
	if err != nil {
		return HostPolicy{}, nil, err
	}
	defer unlock()
	policy, err := u.Store.hostPolicy()
	if err != nil {
		return policy, nil, err
	}
	prior, err := u.Store.hostTarget()
	if err != nil {
		return policy, prior, err
	}
	if policy.Revision != input.PolicyRevision {
		return policy, prior, errors.New("update: host policy revision changed")
	}
	return policy, prior, nil
}

func targetSubmissionSame(prior *hostTargetIntent, input HostTargetSubmission) bool {
	return prior != nil && reflect.DeepEqual(prior.Submission, input)
}

func targetSubmissionCAS(prior *hostTargetIntent, input HostTargetSubmission) error {
	revision := uint64(0)
	if prior != nil {
		revision = prior.Status.Revision
	}
	if revision != input.ExpectedRevision {
		return errors.New("update: local target revision changed")
	}
	if prior != nil && targetActive(prior.Status.Phase) {
		return errors.New("update: cancel the current local target before replacing it")
	}
	return nil
}

func (u *HostUpdater) SubmitLocalTarget(ctx context.Context, input HostTargetSubmission) (HostTargetStatus, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u.observed, u.limit = release.Build{}, nil
	policy, prior, err := u.validateTargetSnapshot(ctx, input, false)
	if err != nil {
		return HostTargetStatus{}, err
	}
	if targetSubmissionSame(prior, input) && prior.Status.Phase != HostTargetCancelled {
		return prior.Status, nil
	}
	if err := targetSubmissionCAS(prior, input); err != nil {
		return HostTargetStatus{}, err
	}
	if u.Engine == nil || u.Helper == nil {
		return HostTargetStatus{}, errors.New("update: local target installation services unavailable")
	}
	journal, journalErr := updateinstall.ReadContext(ctx, u.Store.stateDir())
	if journalErr != nil && !errors.Is(journalErr, os.ErrNotExist) {
		return HostTargetStatus{}, hostError(journalErr)
	}
	if journalErr == nil && !hostFinished(journal.Phase) {
		return HostTargetStatus{}, updateinstall.ErrConflict
	}
	if err := u.targetAuthority(ctx); err != nil {
		return HostTargetStatus{}, err
	}
	prepared, err := u.planTarget(ctx, input, policy, nil, false)
	if err != nil {
		return HostTargetStatus{}, err
	}
	if err := u.Helper(ctx, prepared.proof.Source); err != nil {
		return HostTargetStatus{}, hostError(err)
	}
	intent := &hostTargetIntent{Schema: 1, Submission: input,
		Status: HostTargetStatus{Schema: 1, Revision: input.ExpectedRevision + 1, PolicyRevision: policy.Revision, Phase: HostTargetReserved, CurrentDigest: input.CurrentDigest, RunningDigest: input.CurrentDigest, Target: prepared.plan.Target, Next: prepared.plan.Next, PendingSince: u.now()}}
	return u.reserveTarget(ctx, policy, prior, intent, prepared)
}

func (u *HostUpdater) targetAuthority(ctx context.Context) error {
	reason, err := u.yield(ctx)
	if err != nil {
		return err
	}
	if reason != "" {
		return errors.New("update: local target waits for " + reason)
	}
	return nil
}

func (u *HostUpdater) PlanLocalTarget(ctx context.Context, input HostTargetSubmission) (HostTargetPlan, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u.observed, u.limit = release.Build{}, nil
	policy, prior, err := u.validateTargetSnapshot(ctx, input, true)
	if err != nil {
		return HostTargetPlan{}, err
	}
	if !targetSubmissionSame(prior, input) {
		if err := targetPreviewCAS(prior, input); err != nil {
			return HostTargetPlan{}, err
		}
	}
	prepared, err := u.planTarget(ctx, input, policy, nil, true)
	if err != nil {
		return HostTargetPlan{}, err
	}
	return prepared.plan, nil
}

func targetPreviewCAS(prior *hostTargetIntent, input HostTargetSubmission) error {
	revision := uint64(0)
	if prior != nil {
		revision = prior.Status.Revision
	}
	if revision != input.ExpectedRevision {
		return errors.New("update: local target revision changed")
	}
	return nil
}

func (u *HostUpdater) reserveTarget(ctx context.Context, policy HostPolicy, prior, intent *hostTargetIntent, prepared preparedTarget) (HostTargetStatus, error) {
	engine, err := u.Engine(ctx)
	if err != nil {
		return HostTargetStatus{}, err
	}
	journal, journalErr := engine.Read()
	if journalErr != nil && !errors.Is(journalErr, os.ErrNotExist) {
		return HostTargetStatus{}, hostError(journalErr)
	}
	if journalErr == nil && !hostFinished(journal.Phase) {
		return HostTargetStatus{}, updateinstall.ErrConflict
	}
	generation := uint64(1)
	if journalErr == nil {
		if journal.Request.Generation == math.MaxUint64 {
			return HostTargetStatus{}, errors.New("update: installation generation exhausted")
		}
		generation = journal.Request.Generation + 1
	}
	id, err := newAutomaticID()
	if err != nil {
		return HostTargetStatus{}, err
	}
	request := updateinstall.Request{ID: id, Generation: generation, TargetID: u.ID, Current: prepared.current, Manifest: prepared.next.Manifest}
	artifact, err := request.Manifest.Artifact(request.Current.Platform)
	if err != nil {
		return HostTargetStatus{}, hostError(err)
	}
	owned := &OwnedRequest{Request: id, Generation: generation, PolicyRevision: policy.Revision, IntentRevision: intent.Status.Revision,
		Edge: Edge{From: request.Current.Digest, To: artifact.BinarySHA256, Platform: request.Current.Platform}, Installation: request, Metadata: prepared.proof.Metadata, Epoch: prepared.source.Epoch, Position: prepared.next.Position, Settings: engine.Settings()}
	intent.Work, intent.Status.Request, intent.Status.Generation = owned, id, generation
	return u.commitTargetReservation(ctx, policy, prior, intent)
}

func (u *HostUpdater) commitTargetReservation(ctx context.Context, policy HostPolicy, prior, intent *hostTargetIntent) (HostTargetStatus, error) {
	unlock, err := u.Store.lockContext(ctx)
	if err != nil {
		return HostTargetStatus{}, err
	}
	defer unlock()
	currentPolicy, err := u.Store.hostPolicy()
	if err != nil {
		return HostTargetStatus{}, err
	}
	current, err := u.Store.hostTarget()
	if err != nil {
		return HostTargetStatus{}, err
	}
	state, err := u.Store.hostUpdate()
	if err != nil {
		return HostTargetStatus{}, err
	}
	if currentPolicy.Revision != policy.Revision || !reflect.DeepEqual(current, prior) || state.Owned != nil {
		return HostTargetStatus{}, errors.New("update: local target policy, intent or installation ownership changed during planning")
	}
	if reason, err := u.Store.hostYield(); err != nil || reason != "" {
		return HostTargetStatus{}, errors.Join(err, errors.New("update: another authority holds installation"))
	}
	if err := intent.validate(); err != nil {
		return HostTargetStatus{}, err
	}
	if err := u.Store.checkSavedHostOrder(intent.Work.Installation, u.now()); err != nil {
		return HostTargetStatus{}, err
	}
	if err := u.Store.writeAuthority("host-target.json", intent); err != nil {
		return HostTargetStatus{}, err
	}
	state.Owned, state.Phase, state.Problem, state.NextAttempt = intent.Work, HostInstalling, nil, time.Time{}
	state.Failures = 0
	if err := u.Store.writeAuthority("host-update.json", state); err != nil {
		return HostTargetStatus{}, err
	}
	return intent.Status, nil
}
