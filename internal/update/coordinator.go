package update

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
	"github.com/shaul/mesh/internal/updatenotice"
)

type Plan struct {
	Fleet    Fleet            `json:"fleet"`
	Manifest release.Manifest `json:"manifest"`
}
type Operation struct {
	ID                     string  `json:"id"`
	ExpectedPolicyRevision *uint64 `json:"expectedPolicyRevision,omitempty"`
	CommandID              string  `json:"commandId,omitempty"`
	Generation             uint64  `json:"generation,omitempty"`
	RetryToken             uint64  `json:"retryToken,omitempty"`
}
type Info struct {
	Commands             []updateinstall.CommandCopy `json:"commands,omitempty"`
	AcceptsIdentityFleet bool                        `json:"acceptsIdentityFleet"`
	Health               updateinstall.Health        `json:"health"`
	Installation         *updateinstall.Status       `json:"installation,omitempty"`
}

type Caller interface {
	Call(context.Context, Host, string, any, any) error
}

type Coordinator struct {
	configurationCalls configurationCalls
	SavedFleet         func() (Fleet, error)
	Trusted            func(string) bool
	CheckedRelease     func(context.Context) (*updatenotice.CheckedRelease, error)
	lastAutomaticCheck time.Time
	execution          sync.Mutex
	LegacyExchange     func(context.Context, Host, protocol.Control) (protocol.Control, error)
	ID                 string
	Store              *Store
	Remote             Caller
	Release            release.Client
	CacheDir           string
	Download           func(context.Context, release.Manifest, release.Platform, string) (string, error)
	lastCachePrune     time.Time
}

func (c *Coordinator) Start(ctx context.Context, plan Plan) (Run, error) {
	manifest, err := c.Release.Manifest(ctx, plan.Manifest.Version)
	if err != nil {
		return Run{}, err
	}
	if manifest.Digest() != plan.Manifest.Digest() {
		return Run{}, errors.New("release changed after preview; review it again")
	}
	c.execution.Lock()
	defer c.execution.Unlock()
	return c.Store.Start(c.ID, plan.Fleet, manifest)
}

func (c *Coordinator) Retry(_ context.Context, id string) (Run, error) {
	c.execution.Lock()
	defer c.execution.Unlock()
	return c.Store.Retry(id)
}

func (c *Coordinator) Run(ctx context.Context, report func(error)) {
	ctx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); c.joinConfiguration() }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := c.Step(ctx); err != nil && ctx.Err() == nil && report != nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Coordinator) Step(ctx context.Context) (result error) {
	c.execution.Lock()
	defer func() {
		c.execution.Unlock()
		result = errors.Join(result, c.stepConfiguration(ctx))
	}()
	admissionErr := c.refreshAutomatic(ctx)
	runs, err := c.Store.List()
	if err != nil {
		return errors.Join(admissionErr, err)
	}
	for _, run := range runs {
		if run.statusOnly {
			continue
		}
		if run.Done() && !(run.Origin != nil && !automaticSettled(run)) {
			continue
		}
		if err := c.stepRun(ctx, run); err != nil {
			return errors.Join(admissionErr, err)
		}
	}
	if time.Since(c.lastCachePrune) >= time.Minute {
		if err := c.pruneCache(ctx); err != nil {
			return errors.Join(admissionErr, err)
		}
		c.lastCachePrune = time.Now()
	}
	return admissionErr
}

func (c *Coordinator) stepRun(ctx context.Context, snapshot Run) (err error) {
	defer func() {
		if errors.Is(err, errAutomaticHistory) {
			err = nil
		}
	}()
	run, err := c.Store.Read(snapshot.ID)
	if err != nil {
		return err
	}
	if run.statusOnly {
		return nil
	}
	if run.CoordinatorSetup {
		return nil
	}
	if run.Origin != nil {
		if err := c.checkAutomaticScope(ctx); err != nil && !stoppedAutomaticError(err) {
			return err
		}
	}
	if !run.Cached && !run.Cancel && !run.Stopped {
		if err := c.cache(ctx, run); err != nil {
			return c.stop(run.ID, err)
		}
	}
	for index := range run.Targets {
		current, err := c.Store.Read(run.ID)
		if err != nil {
			return err
		}
		if current.statusOnly {
			return nil
		}
		target := current.Targets[index]
		if settled(target.State) && !target.JoinedUnsettled && !target.StageUnsettled && !target.RetryUnsettled || time.Now().Before(target.RetryAt) {
			continue
		}
		if current.Stopped && !target.Grant && !current.Cancel && target.State == Failed && !target.StageUnsettled && !target.RetryUnsettled {
			continue
		}
		if err := c.reconcile(ctx, current, index); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) cache(ctx context.Context, run Run) error {
	download := c.Download
	if download == nil {
		download = c.Release.Download
	}
	for _, artifact := range run.Release.Artifacts {
		if _, err := download(ctx, run.Release, artifact.Platform, c.CacheDir); err != nil {
			return fmt.Errorf("pin release artifacts: %w", err)
		}
	}
	_, err := c.Store.Change(run.ID, func(r *Run) error { r.Cached = true; return nil })
	return err
}

func settled(state State) bool { return state == Updated || state == Newer || state == Cancelled }

func (c *Coordinator) reconcile(ctx context.Context, run Run, index int) error {
	target := run.Targets[index]
	if run.Origin != nil && run.Cancel && target.StageAttempt == 0 && !target.Grant && target.InstallationID == "" {
		return c.record(run.ID, index, func(t *Target) { t.State = Cancelled; t.RetryPending = false; t.Problem = "" })
	}
	var info Info
	inspectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := c.Remote.Call(inspectCtx, target.Host, "info", nil, &info)
	cancel()
	if err != nil {
		if run.Origin != nil && IsLegacyError(err) {
			return c.fail(run.ID, index, errors.New("automatic direct transition blocked; review a manual bridge on this host"))
		}
		if IsLegacyError(err) && run.Origin == nil {
			return c.bootstrap(ctx, run, index)
		}
		return c.recordError(run.ID, index, err)
	}
	if run.Origin != nil && !automaticStableBuild(info.Health.Build) {
		return c.fail(run.ID, index, errors.New("automatic updates require a stable executing build; review this host manually"))
	}
	if run.Origin != nil && info.Health.Build.Platform != target.Host.Platform {
		return c.fail(run.ID, index, errors.New("automatic target platform differs from enrolled fleet"))
	}
	if info.Health.HostID != target.Host.ID {
		return c.fail(run.ID, index, errors.New("target health identity mismatch"))
	}
	if run.Stopped && !target.Grant && !run.Cancel && !target.StageUnsettled && !target.RetryUnsettled {
		if _, err := c.observeBuild(run, index, info); err != nil {
			return c.fail(run.ID, index, err)
		}
		return nil
	}
	if info.Installation != nil && (info.Installation.Request.ID == run.ID || target.InstallationID != "") {
		return c.reconcileInstallation(ctx, run, index, info)
	}
	if !target.Grant && joinableInstallation(info.Installation, run.ReleaseDigest, target.Host.ID) {
		return c.joinInstallation(ctx, run, index, info)
	}
	if target.Grant {
		return c.fail(run.ID, index, errors.New("authorized installation journal is missing or belongs to another operation"))
	}
	if run.Cancel {
		if target.StageUnsettled || target.RetryUnsettled {
			return c.recordError(run.ID, index, errors.New("issued installation work is not yet visible; cancellation awaits settlement"))
		}
		return c.record(run.ID, index, func(t *Target) { t.State = Cancelled; t.Problem = "" })
	}
	if err := c.prepare(run, index, info); err != nil {
		return c.fail(run.ID, index, err)
	}
	current, err := c.Store.Read(run.ID)
	if err != nil {
		return err
	}
	target = current.Targets[index]
	if settled(target.State) || current.Cancel || current.Stopped {
		return nil
	}
	request := updateinstall.Request{ID: run.ID, TargetID: target.Host.ID, Generation: target.Generation, Manifest: run.Release, Current: *target.Build, Automatic: automaticRequest(current, target)}
	if run.Origin != nil {
		var issued bool
		current, issued, err = c.reserveAutomatic(ctx, run.ID, index, "stage")
		if stoppedAutomaticError(err) {
			return nil
		}
		if err != nil || !issued {
			return err
		}
		target = current.Targets[index]
		request.Automatic = automaticRequest(current, target)
	}
	var status updateinstall.Status
	if err = c.Remote.Call(ctx, target.Host, "stage", request, &status); err != nil {
		return c.recordError(run.ID, index, err)
	}
	if status.Request.ID != run.ID {
		info.Installation = &status
		return c.joinInstallation(ctx, current, index, info)
	}
	if status.Phase == updateinstall.Committed {
		return nil
	}
	info.Installation = &status
	return c.reconcileInstallation(ctx, current, index, info)
}

func (c *Coordinator) observeBuild(run Run, index int, info Info) (bool, error) {
	build := info.Health.Build
	if build.UpdateProtocol != release.CurrentUpdateProtocol {
		return false, errors.New("bootstrap required: host does not support this update protocol")
	}
	artifact, err := run.Release.Artifact(build.Platform)
	if err != nil {
		return false, fmt.Errorf("observe target artifact: %w", err)
	}
	if build.Digest == artifact.BinarySHA256 {
		return true, c.record(run.ID, index, func(t *Target) {
			t.State = Updated
			t.Build = &build
			t.Workers = info.Health.Workers
			t.Commands = info.Commands
			t.Problem = ""
			t.RetryPending = false
			t.BootstrapRetry = false
		})
	}
	comparison, err := release.CompareVersions(build.Version, run.Release.Version)
	if err == nil && comparison > 0 {
		if err := compatibleNewerBuild(build, run.Release.Compatibility); err != nil {
			return false, err
		}
		return true, c.record(run.ID, index, func(t *Target) {
			t.State = Newer
			t.Build = &build
			t.Workers = info.Health.Workers
			t.Commands = info.Commands
			t.Problem = ""
			t.RetryPending = false
			t.BootstrapRetry = false
		})
	}
	return false, nil
}

func (c *Coordinator) prepare(run Run, index int, info Info) error {
	observed, err := c.observeBuild(run, index, info)
	if err != nil || observed {
		return err
	}
	build := info.Health.Build
	if err = run.Release.Allows(build); err != nil {
		return err
	}
	generation := uint64(1)
	if info.Installation != nil {
		if info.Installation.Request.Generation == math.MaxUint64 {
			return errors.New("target installation generation exhausted")
		}
		generation = info.Installation.Request.Generation + 1
	}
	return c.record(run.ID, index, func(t *Target) {
		t.Build = &build
		t.Workers = info.Health.Workers
		t.Commands = info.Commands
		t.Generation = generation
		t.Problem = ""
		t.RetryAt = time.Time{}
	})
}

func (c *Coordinator) reconcileInstallation(ctx context.Context, run Run, index int, info Info) error {
	status := *info.Installation
	target := run.Targets[index]
	installationID := run.ID
	if target.InstallationID != "" {
		installationID = target.InstallationID
	}
	if run.Origin != nil && target.InstallationID == "" && !sameInstallationOrigin(status.Request.Automatic, automaticRequest(run, target)) {
		return c.fail(run.ID, index, errors.New("automatic installation origin differs from approval"))
	}
	if status.Request.ID != installationID || status.Request.TargetID != target.Host.ID || status.Request.Manifest.Digest() != run.ReleaseDigest || status.Request.Generation != target.Generation {
		return c.fail(run.ID, index, errors.New("installation does not match approved operation"))
	}
	var err error
	run, err = c.observeAutomaticDispatch(run, index, status)
	if err != nil {
		return err
	}
	target = run.Targets[index]
	if run.Cancel && target.RetryUnsettled {
		return c.recordError(run.ID, index, errors.New("issued installation retry is not yet visible; cancellation awaits its token"))
	}
	if target.RetryPending {
		return c.reconcileRetry(ctx, run, index, info)
	}
	if status.RetryToken > target.RetryToken {
		if err := c.record(run.ID, index, func(target *Target) { target.RetryToken = status.RetryToken }); err != nil {
			return err
		}
	}
	switch status.Phase {
	case updateinstall.Committed:
		artifact, err := run.Release.Artifact(info.Health.Build.Platform)
		if err != nil || info.Health.Build.Digest != artifact.BinarySHA256 {
			return c.fail(run.ID, index, errors.New("committed target is not executing the approved binary"))
		}
		return c.recordCommitted(run, index, info, status)

	case updateinstall.RolledBack, updateinstall.Failed, updateinstall.RollbackFailed:
		if run.Cancel {
			return c.reconcileCancelledInstallation(ctx, run, index)
		}
		if err := c.record(run.ID, index, func(t *Target) { t.Grant = false }); err != nil {
			return err
		}
		return c.fail(run.ID, index, fmt.Errorf("%s: %s", status.Phase, status.Error))
	case updateinstall.Cancelled:
		return c.record(run.ID, index, func(t *Target) { t.State = Cancelled; t.Grant = false; t.JoinedUnsettled = false; t.Problem = "" })
	case updateinstall.Granted, updateinstall.Activating, updateinstall.Validating, updateinstall.RollingBack:
		if run.Origin != nil && target.InstallationID != "" {
			return c.observeJoinedStaging(run, index, status.Phase)
		}
		return c.record(run.ID, index, func(t *Target) { t.State = Granted; t.Grant = true; t.GrantAccepted = true; t.Problem = status.Error })
	case updateinstall.Accepted, updateinstall.Staged:
		if target.InstallationID != "" {
			return c.observeJoinedStaging(run, index, status.Phase)
		}
		if run.Cancel {
			return c.cancelTarget(ctx, run, index)
		}
		if status.Phase == updateinstall.Accepted {
			return nil
		}
		if err := c.record(run.ID, index, func(t *Target) { t.State = Staged; t.Problem = "" }); err != nil {
			return err
		}
		return c.grant(ctx, run.ID, index)
	default:
		return c.fail(run.ID, index, errors.New("unknown installation state"))
	}
}

func (c *Coordinator) grant(ctx context.Context, id string, index int) error {
	if current, err := c.Store.Read(id); err != nil {
		return err
	} else if current.Origin != nil {
		return c.grantAutomatic(ctx, id, index)
	}
	issued := false
	run, err := c.Store.Change(id, func(r *Run) error {
		if !canGrant(*r, index) {
			return nil
		}
		r.Targets[index].Grant = true
		r.Targets[index].State = Granted
		issued = true
		return nil
	})
	if err != nil || !issued {
		return err
	}
	target := run.Targets[index]
	var status updateinstall.Status
	if err = c.Remote.Call(ctx, target.Host, "grant", Operation{ID: id, Generation: target.Generation}, &status); err != nil {
		return c.recordError(id, index, err)
	}
	return nil
}

func canGrant(run Run, index int) bool {
	if run.Cancel || run.Stopped || !run.Cached {
		return false
	}
	target := run.Targets[index]
	if target.Grant {
		return true
	}
	if !coordinatorBootstrapReady(run) {
		return false
	}
	active, verified := 0, false
	for _, other := range run.Targets {
		if other.Grant {
			active++
		}
		if other.State == Updated || other.State == Newer {
			verified = true
		}
		if blocksDependency(run, target, other) {
			return false
		}
	}
	if active >= 2 || (!verified && active != 0) {
		return false
	}
	return true
}

func coordinatorBootstrapReady(run Run) bool {
	if !run.CoordinatorBootstrap {
		return true
	}
	for _, target := range run.Targets {
		if target.Host.ID == run.Coordinator {
			return target.State == Updated || target.State == Newer
		}
	}
	return false
}

func blocksDependency(run Run, target, other Target) bool {
	if target.Host.ID == other.Host.ID || settled(other.State) {
		return false
	}
	for _, dependency := range other.Host.DependsOn {
		if dependency == target.Host.ID {
			return true
		}
	}
	return target.Host.ID == run.Coordinator && (other.State != Offline || other.Grant)
}

func (c *Coordinator) cancelTarget(ctx context.Context, run Run, index int) error {
	target := run.Targets[index]
	installationID := run.ID
	if target.InstallationID != "" {
		installationID = target.InstallationID
	}
	var status updateinstall.Status
	err := c.Remote.Call(ctx, target.Host, "install-cancel", Operation{ID: installationID, Generation: target.Generation}, &status)
	if err != nil {
		var remote *RemoteError
		if errors.As(err, &remote) && remote.Problem == updateinstall.ErrAlreadyGranted.Error() {
			return c.recordIssuedCancellation(run.ID, index)

		}
		return c.recordError(run.ID, index, err)
	}
	if status.Request.ID != installationID || status.Request.TargetID != target.Host.ID || status.Request.Generation != target.Generation || status.Request.Manifest.Digest() != run.ReleaseDigest {
		return c.fail(run.ID, index, errors.New("target did not acknowledge cancellation of the approved installation"))
	}
	if status.Phase == updateinstall.RolledBack || status.Phase == updateinstall.Failed || status.Phase == updateinstall.RollbackFailed {
		if err := c.record(run.ID, index, func(t *Target) { t.Grant = false }); err != nil {
			return err
		}
		return c.fail(run.ID, index, fmt.Errorf("%s: %s", status.Phase, status.Error))
	}
	if status.Phase != updateinstall.Cancelled {
		return c.fail(run.ID, index, errors.New("target did not acknowledge cancellation of the approved installation"))
	}
	return c.record(run.ID, index, func(t *Target) { t.State = Cancelled; t.Grant = false; t.JoinedUnsettled = false; t.Problem = "" })
}

func compatibleNewerBuild(build release.Build, compatibility release.Compatibility) error {
	if build.Modified || build.StateVersion < compatibility.StateReadMin || build.StateVersion > compatibility.StateReadMax || build.WorkerProtocol < compatibility.WorkerMin || build.WorkerProtocol > compatibility.WorkerMax {
		return errors.New("newer build has incompatible or unknown state or worker capabilities")
	}
	return nil
}

func (c *Coordinator) record(id string, index int, change func(*Target)) error {
	_, err := c.Store.Change(id, func(r *Run) error { change(&r.Targets[index]); return nil })
	return err
}

func (c *Coordinator) stop(id string, problem error) error {
	_, err := c.Store.Change(id, func(r *Run) error { r.Stopped = true; r.Problem = problem.Error(); return nil })
	return err
}

func (c *Coordinator) fail(id string, index int, problem error) error {
	_, err := c.Store.Change(id, func(r *Run) error {
		r.Stopped = true
		r.Targets[index].State = Failed
		r.Targets[index].Problem = problem.Error()
		return nil
	})
	return err
}

func (c *Coordinator) recordError(id string, index int, problem error) error {
	var remote *RemoteError
	if errors.As(problem, &remote) {
		return c.fail(id, index, problem)
	}
	return c.record(id, index, func(t *Target) {
		t.State = Offline
		t.Problem = problem.Error()
		t.RetryAt = time.Now().Add(offlineRetryDelay(*t))
	})
}

const (
	offlineRetry = 30 * time.Second
	// restartRetry polls a target that is authorized to activate: its daemon
	// restarting onto the new build is the expected cause of the lost call,
	// and a 30-second wait turned every successful update into "offline".
	restartRetry = 2 * time.Second
)

func offlineRetryDelay(target Target) time.Duration {
	if target.Grant {
		return restartRetry
	}
	return offlineRetry
}

func sameInstallationOrigin(a, b *updateinstall.AutomaticReference) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
func (c *Coordinator) grantAutomatic(ctx context.Context, id string, index int) error {
	run, issued, err := c.reserveAutomatic(ctx, id, index, "grant")
	if stoppedAutomaticError(err) {
		return nil
	}
	if err != nil || !issued {
		return err
	}
	target := run.Targets[index]
	var status updateinstall.Status
	if err = c.Remote.Call(ctx, target.Host, "grant", Operation{ID: id, Generation: target.Generation}, &status); err != nil {
		return c.recordError(id, index, err)
	}
	if !sameInstallationOrigin(status.Request.Automatic, automaticRequest(run, target)) {
		return c.fail(id, index, errors.New("automatic grant receipt origin differs from approval"))
	}
	return c.record(id, index, func(t *Target) { t.GrantAccepted = true })
}

func (c *Coordinator) recordCommitted(run Run, index int, info Info, status updateinstall.Status) error {
	return c.record(run.ID, index, func(t *Target) {
		t.State = Updated
		t.GrantAccepted = t.GrantAccepted || t.Grant
		t.JoinedUnsettled = false
		t.Grant = false
		t.Problem = ""
		t.Build = &info.Health.Build
		t.Workers = info.Health.Workers
		t.Commands = info.Commands
		if status.Verified != nil {
			t.InterruptedWorkers = status.Verified.InterruptedWorkers
		}
	})
}
func (c *Coordinator) settleCancelledJoin(run Run, index int) error {
	return c.record(run.ID, index, func(t *Target) { t.State = Cancelled; t.Grant = false; t.JoinedUnsettled = false })
}
func (c *Coordinator) recordIssuedCancellation(id string, index int) error {
	return c.record(id, index, func(t *Target) {
		t.State = Granted
		t.Grant = true
		t.GrantAccepted = true
		t.Problem = "Activation was already authorized and may still finish."
	})
}

func (c *Coordinator) reconcileCancelledInstallation(ctx context.Context, run Run, index int) error {
	if run.Origin != nil && run.Targets[index].InstallationID != "" {
		return c.settleCancelledJoin(run, index)
	}
	return c.cancelTarget(ctx, run, index)
}

func automaticStableBuild(build release.Build) bool {
	if build.Modified {
		return false
	}
	_, err := release.CompareVersions(build.Version, build.Version)
	return err == nil
}
