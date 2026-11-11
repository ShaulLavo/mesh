package update

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

// HostSource is read from authenticated publisher evidence, including the
// reader's epoch-local source position after a format waypoint.
type HostSource struct {
	Epoch    uint32
	Position release.Position
	Manifest release.Manifest
}

type hostHopProof struct {
	Metadata string
	Workers  []string
	Source   release.Manifest
}

func hostError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("host updater: %w", err)
}

type HostUpdater struct {
	Order       *HostOrder
	limit       *HostLimit
	localTarget *hostTargetIntent
	Store       *Store
	ID          string
	Current     func() release.Build
	Engine      func(context.Context) (*updateinstall.Engine, error)
	Source      func(context.Context, release.Build) (HostSource, release.DocumentSource, error)
	Helper      func(context.Context, release.Manifest) error
	Probe       updateinstall.Probe
	Now         func() time.Time
	mu          sync.Mutex
	lastTick    time.Time
	observed    release.Build
}

func (u *HostUpdater) now() time.Time {
	if u.Now != nil {
		return u.Now().UTC()
	}
	return time.Now().UTC()
}

func (u *HostUpdater) Run(ctx context.Context, report func(error)) {
	timer := time.NewTicker(30 * time.Second)
	defer timer.Stop()
	for {
		if err := u.Tick(ctx); err != nil && report != nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func (u *HostUpdater) Tick(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u.observed = release.Build{}
	u.limit = nil
	u.localTarget = nil
	if u.Store == nil || u.Current == nil {
		return errors.New("update: host updater configuration incomplete")
	}
	state, err := u.Store.HostState(ctx)
	if err != nil {
		return fmt.Errorf("update: host attention: %w", err)
	}
	now := u.now()
	if state.Failures == 0 && (u.lastTick.IsZero() || now.Sub(u.lastTick) > time.Minute || now.Before(u.lastTick)) {
		state.NextAttempt = time.Time{}
	}
	u.lastTick = now
	journal, journalErr := updateinstall.ReadContext(ctx, u.Store.stateDir())
	if journalErr != nil && !errors.Is(journalErr, os.ErrNotExist) {
		return u.attention(ctx, state, "journal", journalErr)
	}
	state, done, err := u.reconcile(ctx, state, journal, journalErr == nil)
	if err != nil || done {
		return err
	}
	return u.tickAvailable(ctx, state, journal, journalErr == nil)
}

func sameHostIdentity(a, b release.Build) bool {
	return a.Version == b.Version && a.Commit == b.Commit && a.Digest == b.Digest && a.Platform == b.Platform && a.Modified == b.Modified
}

func (u *HostUpdater) currentBuild() release.Build {
	if u.observed.Digest != "" {
		return u.observed
	}
	return u.Current()
}

func (u *HostUpdater) tickAvailable(ctx context.Context, state HostUpdate, journal updateinstall.Status, exists bool) error {
	policy, err := u.Store.HostPolicy(ctx)
	if err != nil {
		return u.attention(ctx, state, "policy", err)
	}
	if err := u.Order.ReleaseDropped(ctx, policy); err != nil {
		return u.wait(ctx, state, err.Error())
	}
	intent, err := u.loadTarget(ctx)
	if err != nil {
		return u.attention(ctx, state, "local target", err)
	}
	u.localTarget = intent
	if state.Owned != nil {
		return u.resume(ctx, state, policy, journal, exists, nil)
	}
	if intent != nil && targetActive(intent.Status.Phase) {
		return u.tickTarget(ctx, state, policy, intent, journal, exists)
	}
	if policy.Mode != HostAutomatic || state.Phase == HostAttention {
		return nil
	}
	if exists && !hostFinished(journal.Phase) {
		return u.wait(ctx, state, "another installation is unfinished")
	}
	if reason, err := u.yield(ctx); err != nil || reason != "" {
		if err != nil {
			return u.attention(ctx, state, "authority", err)
		}
		return u.wait(ctx, state, reason)
	}
	if u.now().Before(state.NextAttempt) {
		return nil
	}
	return u.discover(ctx, state, policy, journal, exists)
}

func hostFinished(phase updateinstall.Phase) bool {
	return phase == updateinstall.Committed || phase == updateinstall.RolledBack || phase == updateinstall.RollbackFailed || phase == updateinstall.Failed || phase == updateinstall.Cancelled
}

func (u *HostUpdater) yield(ctx context.Context) (string, error) {
	unlock, err := u.Store.lockContext(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	return u.Store.hostYield()
}

func (u *HostUpdater) save(ctx context.Context, state HostUpdate) error {
	unlock, err := u.Store.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	prior, err := u.Store.hostUpdate()
	if err != nil {
		return err
	}
	state.ProblemSince = hostProblemSince(prior, state, u.now())
	return u.Store.writeAuthority("host-update.json", state)
}

func (u *HostUpdater) attention(ctx context.Context, state HostUpdate, kind string, cause error) error {
	if state.Owned == nil {
		state.Phase = HostAttention
	}
	state.Problem = &HostProblem{Kind: kind, Detail: boundedProblem(cause.Error())}
	return errors.Join(cause, u.save(ctx, state))
}

func (u *HostUpdater) wait(ctx context.Context, state HostUpdate, reason string) error {
	if state.Owned == nil {
		state.Phase = HostWaiting
	}
	state.Problem = &HostProblem{Kind: "waiting", Detail: reason}
	return u.save(ctx, state)
}

func (u *HostUpdater) backoff(ctx context.Context, state HostUpdate, cause error) error {
	if errors.Is(cause, release.ErrTrustRefreshBudget) || errors.Is(cause, release.ErrPassExhausted) {
		state.NextAttempt = time.Time{}
		return u.save(ctx, state)
	}
	state.Failures = min(state.Failures+1, 6)
	state.NextAttempt = u.now().Add(min(15*time.Minute*time.Duration(1<<(state.Failures-1)), 6*time.Hour))
	state.Problem = &HostProblem{Kind: "retry", Detail: boundedProblem(cause.Error())}
	if state.Owned == nil {
		state.Phase = HostWaiting
	}
	return errors.Join(cause, u.save(ctx, state))
}

func (u *HostUpdater) reconcile(ctx context.Context, state HostUpdate, journal updateinstall.Status, exists bool) (HostUpdate, bool, error) {
	if !exists {
		return state, false, nil
	}
	owned := state.Owned
	if owned != nil && (owned.Request != journal.Request.ID || owned.Generation != journal.Request.Generation) {
		return u.reconcileForeign(ctx, state, journal)
	}
	if owned != nil {
		if err := owned.matches(journal.Request); err != nil {
			return state, true, u.attention(ctx, state, "ownership", err)
		}
		if owned.Settings != journal.Settings {
			return state, true, u.attention(ctx, state, "settings", errors.New("installation settings differ from owned request"))
		}
	}
	if !hostFinished(journal.Phase) {
		return state, false, nil
	}
	if journal.Phase == updateinstall.RollbackFailed {
		return state, true, u.attention(ctx, state, "rollback", errors.New(journal.Error))
	}
	if journal.Phase == updateinstall.Failed && owned != nil {
		return state, false, nil
	}
	return u.recordFinal(ctx, state, journal)
}

func (u *HostUpdater) reconcileForeign(ctx context.Context, state HostUpdate, journal updateinstall.Status) (HostUpdate, bool, error) {
	if !hostFinished(journal.Phase) {
		return state, true, u.wait(ctx, state, "another installation is unfinished")
	}
	if journal.Request.Generation >= state.Owned.Generation {
		return state, true, u.attention(ctx, state, "ownership", errors.New("journal superseded owned host work"))
	}
	return state, false, nil
}

func (u *HostUpdater) recordFinal(ctx context.Context, state HostUpdate, journal updateinstall.Status) (HostUpdate, bool, error) {
	unlock, err := u.Store.lockContext(ctx)
	if err != nil {
		return state, true, err
	}
	defer unlock()
	if journal.Phase == updateinstall.Committed {
		held, err := u.Store.adoptionOwnsCommit(journal)
		if err != nil {
			return state, true, err
		}
		if held {
			return u.waitAdoptionCommit(state)
		}
	}
	if state.LastRequest == journal.Request.ID && state.LastGeneration == journal.Request.Generation && state.LastPhase == journal.Phase && state.Owned == nil {
		return state, false, nil
	}
	if err := u.recordFinalEvidence(state.Owned, journal); err != nil {
		return state, true, err
	}
	state.Owned, state.Phase, state.Problem = nil, HostIdle, nil
	state.LastRequest, state.LastGeneration = journal.Request.ID, journal.Request.Generation
	state.LastPhase = journal.Phase
	state.NextAttempt, state.Failures = time.Time{}, 0
	return state, false, u.Store.writeAuthority("host-update.json", state)
}

func (u *HostUpdater) recordFinalEvidence(owned *OwnedRequest, journal updateinstall.Status) error {
	if journal.Phase == updateinstall.Committed {
		if err := u.recordCommit(owned, journal); err != nil {
			return err
		}
	}
	if journal.Phase == updateinstall.RolledBack && owned != nil {
		if err := u.recordFailedTarget(*owned, journal); err != nil {
			return err
		}
	}
	if owned != nil && owned.IntentRevision != 0 {
		return u.Store.finishLocalTarget(*owned, journal)
	}
	return nil
}

func (u *HostUpdater) recordFailedTarget(owned OwnedRequest, journal updateinstall.Status) error {
	if journal.Verified == nil || journal.Verified.Build.Digest != owned.Edge.From || journal.Verified.HostID != u.ID {
		return errors.New("update: rollback journal lacks verified source identity")
	}
	failed := FailedEdge{Edge: owned.Edge, Version: journal.Request.Manifest.Version, Phase: journal.Phase, Problem: boundedProblem(journal.Error), At: u.now()}
	return u.Store.writeAuthority(failedEdgeName(failed.Edge), failed)
}

func (u *HostUpdater) waitAdoptionCommit(state HostUpdate) (HostUpdate, bool, error) {
	if state.Owned == nil {
		state.Phase = HostWaiting
	}
	state.Problem = &HostProblem{Kind: "waiting", Detail: "adoption awaits local confirmation of pinned image, policy and preserved state"}
	return state, true, u.Store.writeAuthority("host-update.json", state)
}

func (u *HostUpdater) recordCommit(owned *OwnedRequest, status updateinstall.Status) error {
	if owned != nil && owned.IntentRevision != 0 {
		if _, err := u.Store.localTargetWork(*owned); err != nil {
			return err
		}
	}
	artifact, err := status.Request.Manifest.Artifact(status.Request.Current.Platform)
	if err != nil {
		return hostError(err)
	}
	if status.Verified == nil || status.Verified.HostID != u.ID || status.Verified.Build.Digest != artifact.BinarySHA256 {
		return errors.New("update: committed journal lacks verified installed identity")
	}
	entry := LedgerEntry{Request: status.Request.ID, Generation: status.Request.Generation, Version: status.Request.Manifest.Version, Binary: artifact.BinarySHA256,
		From: status.Request.Current.Digest, Origin: string(HostManual), CommittedAt: status.UpdatedAt, Workers: len(status.Original.Workers), WorkersPreserved: preservedHostWorkers(status)}
	if owned != nil {
		entry.Metadata = owned.Metadata
		if owned.IntentRevision == 0 {
			entry.Origin = hostOrigin
		}
	}
	if entry.WorkersPreserved != entry.Workers && !hostRebooted(status) {
		return errors.New("update: committed journal did not preserve every worker")
	}
	if err := u.Store.appendLedger(entry); err != nil {
		return err
	}
	var identity hostLedgerIdentity
	if err := u.Store.readAuthority(ledgerIdentityName(entry), &identity); err != nil {
		return err
	}
	entry.Seq = identity.Seq
	if err := u.Store.pruneFailedSource(entry.From); err != nil {
		return err
	}
	return u.Store.recordAdoptionMilestone(entry)
}

func hostRebooted(status updateinstall.Status) bool {
	return status.Verified != nil && status.Original.BootID != "" && status.Verified.BootID != "" && status.Original.BootID != status.Verified.BootID
}

func preservedHostWorkers(status updateinstall.Status) int {
	preserved := 0
	for _, original := range status.Original.Workers {
		if hostWorkerPreserved(original, status.Verified.Workers) {
			preserved++
		}
	}
	return preserved
}

func hostWorkerPreserved(original updateinstall.Worker, workers []updateinstall.Worker) bool {
	for _, worker := range workers {
		if worker.ID == original.ID && worker.PID == original.PID && worker.ShellPID == original.ShellPID {
			return true
		}
	}
	return false
}

func (s *Store) recordAdoptionMilestone(entry LedgerEntry) error {
	var adoption HostAdoption
	err := s.readAuthority("host-adoption.json", &adoption)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := adoption.validate(); err != nil {
		return err
	}
	if adoption.Settled || !adoption.Confirmed || adoption.Unattended != nil || entry.Origin != hostOrigin || entry.Binary == adoption.Adopted.Binary {
		return nil
	}
	adoption.Unattended = &entry
	if err := adoption.validate(); err != nil {
		return err
	}
	return s.writeAuthority("host-adoption.json", adoption)
}

func (u *HostUpdater) resume(ctx context.Context, state HostUpdate, policy HostPolicy, journal updateinstall.Status, exists bool, verified *hostHopProof) error {
	owned := state.Owned
	if exists && journal.Request.ID == owned.Request && journal.Request.Generation == owned.Generation && journal.Phase != updateinstall.Accepted && journal.Phase != updateinstall.Staged && journal.Phase != updateinstall.Failed {
		return nil
	}
	if !u.targetResumeAllowed(policy, *owned) || policy.Revision != owned.PolicyRevision || !sameHostIdentity(u.Current(), owned.Installation.Current) {
		return u.retireUngrantable(ctx, state, journal, exists)
	}
	if u.now().Before(state.NextAttempt) {
		return nil
	}
	if err := u.targetAuthority(ctx); err != nil {
		return u.wait(ctx, state, err.Error())
	}
	limit, err := u.Order.Refresh(ctx, policy, owned.Installation.Current, u.now())
	if err != nil {
		return u.wait(ctx, state, err.Error())
	}
	u.limit = limit
	if !limit.allows(owned.Installation.Manifest.Version) || limit.failed(owned.Edge) {
		return u.wait(ctx, state, "predecessor evidence holds the owned hop")
	}
	if u.Engine == nil {
		return u.attention(ctx, state, "engine", errors.New("host installation engine unavailable"))
	}
	engine, err := u.Engine(ctx)
	if err != nil {
		return u.backoff(ctx, state, err)
	}
	if engine.Settings() != owned.Settings {
		return u.attention(ctx, state, "settings", errors.New("host installation settings changed before acceptance"))
	}
	if verified == nil {
		proof, err := u.verifyOwned(ctx, *owned)
		if err != nil {
			return u.backoff(ctx, state, err)
		}
		verified = &proof
	}
	return u.stageAndGrant(ctx, state, engine, *verified, journal, exists)
}

func (u *HostUpdater) stageAndGrant(ctx context.Context, state HostUpdate, engine *updateinstall.Engine, verified hostHopProof, journal updateinstall.Status, exists bool) error {
	owned := state.Owned
	engine = engine.WithHostOrigin().WithAcceptance(u.acceptWorkers(verified))
	var err error
	if exists && journal.Request.ID == owned.Request && journal.Phase == updateinstall.Failed {
		_, err = engine.Retry(ctx, owned.Request, owned.Generation)
	} else {
		_, err = engine.Stage(ctx, owned.Installation)
	}
	if err != nil {
		return u.backoff(ctx, state, err)
	}
	if err := u.Helper(ctx, verified.Source); err != nil {
		return u.backoff(ctx, state, fmt.Errorf("verify helper before grant: %w", err))
	}
	_, err = engine.Grant(ctx, owned.Request, owned.Generation)
	if err != nil {
		return u.backoff(ctx, state, err)
	}
	return nil
}

func (u *HostUpdater) verifyOwned(ctx context.Context, owned OwnedRequest) (hostHopProof, error) {
	if u.Source == nil || u.Helper == nil || u.Probe == nil {
		return hostHopProof{}, errors.New("update: owned request verification unavailable")
	}
	source, documents, err := u.Source(ctx, u.currentBuild())
	if err != nil {
		return hostHopProof{}, err
	}
	if err := verifyHostSource(u.currentBuild(), source); err != nil {
		return hostHopProof{}, err
	}
	budget := release.NewPassBudget()
	ref := release.AssetRef{Tag: owned.Installation.Manifest.Version, Name: release.UpdateMetadataAssetName, SHA256: owned.Metadata}
	data, err := release.ReadDocument(ctx, documents, ref, release.MetadataDocument, &budget)
	if err != nil {
		return hostHopProof{}, hostError(err)
	}
	metadata, err := release.AuthenticateMetadata(ctx, documents, ref, data, owned.Epoch)
	if err != nil {
		return hostHopProof{}, hostError(err)
	}
	health, err := u.Probe(ctx)
	if err != nil {
		return hostHopProof{}, err
	}
	if health.HostID != u.ID || health.Build != owned.Installation.Current {
		return hostHopProof{}, errors.New("update: owned request source is no longer running")
	}
	u.observed = health.Build
	next := release.PlannedRelease{Metadata: &metadata, Manifest: owned.Installation.Manifest}
	for _, transition := range next.Manifest.Compatibility.Transitions {
		if transition.Platform == owned.Edge.Platform && transition.FromDigest == owned.Edge.From && transition.ToDigest == owned.Edge.To {
			next.Transition = transition
		}
	}
	proof, err := verifyHostHop(ctx, u.currentBuild(), health.Workers, next, data, documents)
	if err != nil {
		return hostHopProof{}, err
	}
	if err := u.Helper(ctx, source.Manifest); err != nil {
		return hostHopProof{}, err
	}
	proof.Source = source.Manifest
	return proof, nil
}

func (u *HostUpdater) acceptWorkers(proof hostHopProof) updateinstall.Acceptance {
	authorize := u.Store.AuthorizeHostAcceptance(nil)
	return func(ctx context.Context, request updateinstall.Request, kind updateinstall.AcceptanceKind, commit func() error) error {
		if err := u.checkAcceptanceWorkers(ctx, request, proof.Workers); err != nil {
			return err
		}
		return authorize(ctx, request, kind, func() error {
			return u.Store.commitHostOrder(request, u.now(), commit)
		})
	}
}

func (u *HostUpdater) checkAcceptanceWorkers(ctx context.Context, request updateinstall.Request, proved []string) error {
	health, err := u.Probe(ctx)
	if err != nil {
		return err
	}
	if health.HostID != request.TargetID || health.Build != request.Current {
		return errors.New("update: host source changed before acceptance")
	}
	for _, worker := range health.Workers {
		if err := verifyHostWorker(worker, request.Manifest.Compatibility, proved); err != nil {
			return err
		}
	}
	return nil
}

func (u *HostUpdater) retireUngrantable(ctx context.Context, state HostUpdate, journal updateinstall.Status, exists bool) error {
	owned := *state.Owned
	if exists && journal.Request.ID == state.Owned.Request && (journal.Phase == updateinstall.Accepted || journal.Phase == updateinstall.Staged) {
		engine, err := u.Engine(ctx)
		if err != nil {
			return u.attention(ctx, state, "engine", err)
		}
		if _, err := engine.Cancel(ctx, journal.Request.ID, journal.Request.Generation); err != nil {
			return hostError(err)
		}
	}
	state.Owned, state.Phase, state.Problem = nil, HostIdle, nil
	return u.retireLocalTarget(ctx, state, owned)
}

func (u *HostUpdater) discover(ctx context.Context, state HostUpdate, policy HostPolicy, journal updateinstall.Status, exists bool) error {
	current := u.currentBuild()
	if current.Modified || !hostDigest(current.Digest) {
		return u.wait(ctx, state, "modified or unidentified build requires manual updates")
	}
	if _, err := release.CompareVersions(current.Version, current.Version); err != nil {
		return u.wait(ctx, state, "development build requires manual updates")
	}
	if u.Source == nil || u.Probe == nil || u.Helper == nil {
		return u.attention(ctx, state, "configuration", errors.New("host verification services unavailable"))
	}
	return u.discoverObserved(ctx, state, policy, current, journal, exists)
}

func (u *HostUpdater) discoverObserved(ctx context.Context, state HostUpdate, policy HostPolicy, current release.Build, journal updateinstall.Status, exists bool) error {
	health, err := u.Probe(ctx)
	if err != nil {
		return u.backoff(ctx, state, err)
	}
	if health.HostID != u.ID || !sameHostIdentity(health.Build, current) {
		return u.attention(ctx, state, "installation", errors.New("executing host identity differs from local observation"))
	}
	u.observed, current = health.Build, health.Build
	source, documents, err := u.Source(ctx, current)
	if err != nil {
		return u.backoff(ctx, state, err)
	}
	if err := verifyHostSource(current, source); err != nil {
		return u.attention(ctx, state, "source", err)
	}
	var cursor DiscoveryCursor
	if err := u.Store.readAuthority("host-discovery.json", &cursor); err != nil && !errors.Is(err, os.ErrNotExist) {
		return u.attention(ctx, state, "discovery", err)
	}
	return u.discoverOrdered(ctx, state, policy, current, source, documents, cursor, journal, exists)
}

func (u *HostUpdater) discoverOrdered(ctx context.Context, state HostUpdate, policy HostPolicy, current release.Build, source HostSource, documents release.DocumentSource, cursor DiscoveryCursor, journal updateinstall.Status, exists bool) error {
	limit, err := u.Order.Refresh(ctx, policy, current, u.now())
	if err != nil {
		return u.wait(ctx, state, err.Error())
	}
	u.limit = limit
	cursor.Route.Failed, err = u.Store.retainHostRouteFailures(cursor.Route.Failed, limit)
	if err != nil {
		return u.attention(ctx, state, "failed edge", err)
	}
	step, selected, err := hostLimitedDiscoveryPass(ctx, current, source, cursor, documents, limit)
	if err != nil {
		return u.backoff(ctx, state, err)
	}
	if err := u.saveCursor(ctx, step.Cursor); err != nil {
		return err
	}
	if err := u.observeDiscovery(ctx, current, source, step, selected); err != nil {
		return err
	}
	if limit != nil && !newerHostVersion(limit.Version, current.Version) && len(selected) > 0 {
		state.NextAttempt = u.now().Add(hostCheckDelay())
		return u.wait(ctx, state, "predecessors have no newer soaked release")
	}
	if step.Next == nil {
		state.Phase, state.Problem = HostDiscovering, nil
		if !step.Exhausted {
			state.NextAttempt = u.now().Add(hostCheckDelay())
		}
		return u.save(ctx, state)
	}
	return u.prepare(ctx, state, policy, source, documents, step, selected, journal, exists)
}

func hostCheckDelay() time.Duration {
	jitter, err := rand.Int(rand.Reader, big.NewInt(int64(5*time.Minute)))
	if err != nil {
		return 30 * time.Minute
	}
	return 30*time.Minute + time.Duration(jitter.Int64())
}

func hostLimitedDiscoveryPass(ctx context.Context, current release.Build, source HostSource, cursor DiscoveryCursor, documents release.DocumentSource, limit *HostLimit) (DiscoveryStep, []byte, error) {
	step := DiscoveryStep{Cursor: cursor, Budget: release.NewPassBudget()}
	if err := discoverHead(ctx, source.Epoch, &step, documents); err != nil {
		if errors.Is(err, release.ErrPassExhausted) {
			step.Exhausted = true
			return step, nil, nil
		}
		return step, nil, err
	}
	metadata, err := discoveryMetadata(ctx, source.Epoch, &step, documents)
	if err != nil {
		if errors.Is(err, release.ErrPassExhausted) {
			step.Exhausted = true
			return step, nil, nil
		}
		return step, nil, err
	}
	metadata, selected, err := hostLimitMetadata(ctx, source.Epoch, metadata, step.Cursor.Metadata, documents, &step.Budget, limit)
	if err != nil {
		return step, nil, err
	}
	order, err := release.CompareVersions(metadata.Version, current.Version)
	if err != nil || order <= 0 {
		return step, selected, hostError(err)
	}
	route, err := release.NextHop(ctx, current, source.Position, metadata, step.Cursor.Route, documents, step.Budget)
	step.Cursor.Route, step.Next, step.Budget = route.Cursor, route.Next, route.Budget
	step.Exhausted, step.Waiting = route.Exhausted, route.Waiting
	return step, selected, hostError(err)
}

func verifyHostSource(current release.Build, source HostSource) error {
	if source.Epoch == 0 {
		return errors.New("update: authenticated source reader position unavailable")
	}
	if err := source.Manifest.Validate(); err != nil {
		return hostError(err)
	}
	artifact, err := source.Manifest.Artifact(current.Platform)
	if err != nil {
		return hostError(err)
	}
	if artifact.BinarySHA256 != current.Digest || source.Manifest.Version != current.Version || source.Manifest.Commit != current.Commit {
		return errors.New("update: executing build differs from official source artifact")
	}
	return nil
}

func (u *HostUpdater) saveCursor(ctx context.Context, cursor DiscoveryCursor) error {
	unlock, err := u.Store.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return u.Store.writeAuthority("host-discovery.json", cursor)
}

func (u *HostUpdater) prepare(ctx context.Context, state HostUpdate, policy HostPolicy, source HostSource, documents release.DocumentSource, step DiscoveryStep, selected []byte, journal updateinstall.Status, exists bool) error {
	next := *step.Next
	artifact, err := next.Manifest.Artifact(u.currentBuild().Platform)
	if err != nil {
		return u.backoff(ctx, state, err)
	}
	edge := Edge{u.currentBuild().Digest, artifact.BinarySHA256, u.currentBuild().Platform}
	failed, err := u.Store.failedHostEdge(edge)
	failed = failed || u.limit.failed(edge)
	if err != nil {
		return u.attention(ctx, state, "failed edge", err)
	}
	if failed {
		step.Cursor.Route.Failed = []release.FailedEdge{{FromDigest: edge.From, ToDigest: edge.To, Platform: edge.Platform}}
		if err := u.saveCursor(ctx, step.Cursor); err != nil {
			return err
		}
		return u.wait(ctx, state, "failed edge quarantined; searching for a verified alternative")
	}
	health, err := u.Probe(ctx)
	if err != nil {
		return u.backoff(ctx, state, err)
	}
	if health.HostID != u.ID || health.Build != u.currentBuild() {
		return u.attention(ctx, state, "installation", errors.New("running host differs from updater source"))
	}
	metadataBytes := selected
	if pending := step.Cursor.Route.Search; pending != nil && pending.Pending != nil && len(pending.Pending.Metadata) > 0 {
		metadataBytes = pending.Pending.Metadata
	}
	proof, err := verifyHostHop(ctx, u.currentBuild(), health.Workers, next, metadataBytes, documents)
	if err != nil {
		return u.backoff(ctx, state, err)
	}
	if err := u.Helper(ctx, source.Manifest); err != nil {
		return u.backoff(ctx, state, fmt.Errorf("verify current running helper: %w", err))
	}
	proof.Source = source.Manifest
	return u.issue(ctx, state, policy, source, next, edge, proof, journal, exists)
}

func (u *HostUpdater) issue(ctx context.Context, state HostUpdate, policy HostPolicy, source HostSource, next release.PlannedRelease, edge Edge, proof hostHopProof, journal updateinstall.Status, exists bool) error {
	engine, err := u.Engine(ctx)
	if err != nil {
		return u.backoff(ctx, state, err)
	}
	settings := engine.Settings()
	if settings.ClientOnly {
		return u.attention(ctx, state, "installation", errors.New("host updater requires a managed daemon service"))
	}
	if exists && journal.Request.Generation == math.MaxUint64 {
		return u.attention(ctx, state, "generation", errors.New("installation generation exhausted"))
	}
	id, err := newAutomaticID()
	if err != nil {
		return err
	}
	generation := uint64(1)
	if exists {
		generation = journal.Request.Generation + 1
	}
	request := updateinstall.Request{ID: id, TargetID: u.ID, Generation: generation, Manifest: next.Manifest, Current: u.currentBuild()}
	state.Owned = &OwnedRequest{Request: id, Generation: generation, PolicyRevision: policy.Revision, Edge: edge, Installation: request, Metadata: proof.Metadata, Position: next.Position, Epoch: source.Epoch, Settings: settings}
	state.Phase, state.Problem, state.NextAttempt = HostInstalling, nil, time.Time{}
	if err := u.reserve(ctx, policy, state); err != nil {
		return err
	}
	return u.resume(ctx, state, policy, journal, exists, &proof)
}

func (u *HostUpdater) reserve(ctx context.Context, policy HostPolicy, state HostUpdate) error {
	unlock, err := u.Store.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := u.Store.hostPolicy()
	if err != nil {
		return err
	}
	prior, err := u.Store.hostUpdate()
	if err != nil {
		return err
	}
	if current.Mode != HostAutomatic || current.Revision != policy.Revision || prior.Owned != nil {
		return errors.New("update: host policy or ownership changed during discovery")
	}
	if reason, err := u.Store.hostYield(); err != nil || reason != "" {
		return errors.Join(err, fmt.Errorf("update: host held: %s", reason))
	}
	return u.Store.writeAuthority("host-update.json", state)
}

func verifyHostHop(ctx context.Context, current release.Build, workers []updateinstall.Worker, next release.PlannedRelease, data []byte, documents release.DocumentSource) (hostHopProof, error) {
	if next.Metadata == nil {
		return hostHopProof{}, errors.New("update: hop has no authenticated update metadata")
	}
	metadata := *next.Metadata
	ref := release.AssetRef{Tag: metadata.Version, Name: release.UpdateMetadataAssetName, SHA256: discoveryDigest(data)}
	verified, err := release.AuthenticateMetadata(ctx, documents, ref, data, metadata.Epoch)
	if err != nil {
		return hostHopProof{}, hostError(err)
	}
	if verified.Manifest != metadata.Manifest || verified.Version != next.Manifest.Version {
		return hostHopProof{}, errors.New("update: authenticated hop changed")
	}
	if verified.Updater == nil {
		return hostHopProof{}, errors.New("update: autonomous target lacks authenticated updater capability")
	}
	budget := release.NewPassBudget()
	manifest, err := release.ReadDocument(ctx, documents, release.AssetRef{Tag: verified.Version, Name: release.ManifestAssetName, SHA256: verified.Manifest}, release.ManifestDocument, &budget)
	if err != nil {
		return hostHopProof{}, hostError(err)
	}
	var frozen release.Manifest
	if err := json.Unmarshal(manifest, &frozen); err != nil {
		return hostHopProof{}, fmt.Errorf("update: decode hop manifest: %w", err)
	}
	if frozen.Digest() != next.Manifest.Digest() {
		return hostHopProof{}, errors.New("update: signed hop manifest differs")
	}
	if err := frozen.Allows(current); err != nil {
		return hostHopProof{}, hostError(err)
	}
	order, err := release.CompareVersions(frozen.Version, current.Version)
	if err != nil || order <= 0 {
		return hostHopProof{}, errors.New("update: host hop must strictly advance a stable release")
	}
	return verifyHostRollback(ctx, current, workers, next, verified, ref.SHA256, documents, &budget)
}

func verifyHostRollback(ctx context.Context, current release.Build, workers []updateinstall.Worker, next release.PlannedRelease, metadata release.UpdateMetadata, metadataDigest string, documents release.DocumentSource, budget *release.PassBudget) (hostHopProof, error) {
	transition := next.Transition
	artifact, err := next.Manifest.Artifact(current.Platform)
	if err != nil {
		return hostHopProof{}, hostError(err)
	}
	if transition.FromDigest != current.Digest || transition.ToDigest != artifact.BinarySHA256 || transition.Platform != current.Platform {
		return hostHopProof{}, errors.New("update: hop transition differs from exact running edge")
	}
	forward, err := release.ReadDocument(ctx, documents, release.AssetRef{Tag: next.Manifest.Version, Name: transition.Proof + ".json", SHA256: transition.Proof}, release.ReceiptDocument, budget)
	if err != nil {
		return hostHopProof{}, hostError(err)
	}
	if err := verifyHostForwardRanges(current, next, forward); err != nil {
		return hostHopProof{}, err
	}
	rollback := ""
	for _, proof := range metadata.Rollback {
		if proof.Forward == transition.Proof {
			rollback = proof.Rollback
		}
	}
	if rollback == "" {
		return hostHopProof{}, errors.New("update: exact rollback proof missing")
	}
	data, err := release.ReadDocument(ctx, documents, release.AssetRef{Tag: next.Manifest.Version, Name: rollback + ".rollback.json", SHA256: rollback}, release.ReceiptDocument, budget)
	if err != nil {
		return hostHopProof{}, hostError(err)
	}
	proof, err := release.VerifyRollbackProof(data, rollback, forward, transition, next.Manifest.Compatibility)
	if err != nil {
		return hostHopProof{}, hostError(err)
	}
	for _, worker := range workers {
		if err := verifyHostWorker(worker, next.Manifest.Compatibility, proof.Workers); err != nil {
			return hostHopProof{}, err
		}
	}
	return hostHopProof{Metadata: metadataDigest, Workers: proof.Workers}, nil
}

func verifyHostForwardRanges(current release.Build, next release.PlannedRelease, forward []byte) error {
	receipt, err := release.VerifyTransitionReceipt(forward, next.Transition, next.Manifest.Compatibility)
	if err != nil {
		return hostError(err)
	}
	if current.StateVersion < receipt.StateReadMin || current.StateVersion > receipt.StateReadMax || current.WorkerProtocol < receipt.WorkerMin || current.WorkerProtocol > receipt.WorkerMax {
		return errors.New("update: executing source is outside its exact forward receipt's tested ranges")
	}
	return nil
}

func verifyHostWorker(worker updateinstall.Worker, compatibility release.Compatibility, proved []string) error {
	if worker.Protocol < compatibility.WorkerMin || worker.Protocol > compatibility.WorkerMax || worker.Build == nil || !hostDigest(worker.Build.Digest) {
		return fmt.Errorf("update: worker %s has unknown or incompatible retained implementation", worker.ID)
	}
	for _, digest := range proved {
		if digest == worker.Build.Digest {
			return nil
		}
	}
	return fmt.Errorf("update: worker %s lacks exact rollback implementation proof", worker.ID)
}
