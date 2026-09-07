package update

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

type Plan struct {
	Fleet    Fleet            `json:"fleet"`
	Manifest release.Manifest `json:"manifest"`
}
type Operation struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation,omitempty"`
	RetryToken uint64 `json:"retryToken,omitempty"`
}
type Info struct {
	Health       updateinstall.Health  `json:"health"`
	Installation *updateinstall.Status `json:"installation,omitempty"`
}

type Caller interface {
	Call(context.Context, Host, string, any, any) error
}

type Coordinator struct {
	execution      sync.Mutex
	LegacyExchange func(context.Context, Host, protocol.Control) (protocol.Control, error)
	ID             string
	Store          *Store
	Remote         Caller
	Release        release.Client
	CacheDir       string
	Download       func(context.Context, release.Manifest, release.Platform, string) (string, error)
}

func (c *Coordinator) Start(ctx context.Context, plan Plan) (Run, error) {
	manifest, err := c.Release.Manifest(ctx, plan.Manifest.Version)
	if err != nil {
		return Run{}, err
	}
	if manifest.Digest() != plan.Manifest.Digest() {
		return Run{}, errors.New("release changed after preview; review it again")
	}
	return c.Store.Start(c.ID, plan.Fleet, manifest)
}

func (c *Coordinator) Retry(_ context.Context, id string) (Run, error) {
	c.execution.Lock()
	defer c.execution.Unlock()
	return c.Store.Retry(id)
}

func (c *Coordinator) Run(ctx context.Context, report func(error)) {
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

func (c *Coordinator) Step(ctx context.Context) error {
	c.execution.Lock()
	defer c.execution.Unlock()
	runs, err := c.Store.List()
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Done() {
			continue
		}
		if err := c.stepRun(ctx, run); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) stepRun(ctx context.Context, run Run) error {
	if run.CoordinatorSetup {
		return nil
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
		target := current.Targets[index]
		if settled(target.State) || time.Now().Before(target.RetryAt) {
			continue
		}
		if current.Stopped && !target.Grant && !current.Cancel {
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
	var info Info
	inspectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := c.Remote.Call(inspectCtx, target.Host, "info", nil, &info)
	cancel()
	if err != nil {
		if IsLegacyError(err) {
			return c.bootstrap(ctx, run, index)
		}
		return c.recordError(run.ID, index, err)
	}
	if info.Health.HostID != target.Host.ID {
		return c.fail(run.ID, index, errors.New("target health identity mismatch"))
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
	request := updateinstall.Request{ID: run.ID, TargetID: target.Host.ID, Generation: target.Generation, Manifest: run.Release, Current: *target.Build}
	var status updateinstall.Status
	if err = c.Remote.Call(ctx, target.Host, "stage", request, &status); err != nil {
		return c.recordError(run.ID, index, err)
	}
	if status.Request.ID != run.ID {
		return c.joinInstallation(ctx, current, index, Info{Health: info.Health, Installation: &status})
	}
	if status.Phase == updateinstall.Committed {
		return nil
	}
	return c.reconcileInstallation(ctx, current, index, Info{Health: info.Health, Installation: &status})
}

func (c *Coordinator) prepare(run Run, index int, info Info) error {
	build := info.Health.Build
	if build.UpdateProtocol != release.CurrentUpdateProtocol {
		return errors.New("bootstrap required: host does not support this update protocol")
	}
	artifact, err := run.Release.Artifact(build.Platform)
	if err != nil {
		return err
	}
	if build.Digest == artifact.BinarySHA256 {
		return c.record(run.ID, index, func(t *Target) { t.State = Updated; t.Build = &build; t.Workers = info.Health.Workers; t.Problem = "" })
	}
	comparison, err := release.CompareVersions(build.Version, run.Release.Version)
	if err == nil && comparison > 0 {
		if err := compatibleNewerBuild(build, run.Release.Compatibility); err != nil {
			return err
		}
		return c.record(run.ID, index, func(t *Target) { t.State = Newer; t.Build = &build; t.Workers = info.Health.Workers; t.Problem = "" })
	}
	if err = run.Release.Allows(build); err != nil {
		return err
	}
	generation := uint64(1)
	if info.Installation != nil {
		generation = info.Installation.Request.Generation + 1
	}
	return c.record(run.ID, index, func(t *Target) {
		t.Build = &build
		t.Workers = info.Health.Workers
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
	if status.Request.ID != installationID || status.Request.TargetID != target.Host.ID || status.Request.Manifest.Digest() != run.ReleaseDigest || status.Request.Generation != target.Generation {
		return c.fail(run.ID, index, errors.New("installation does not match approved operation"))
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
		return c.record(run.ID, index, func(t *Target) {
			t.State = Updated
			t.Grant = false
			t.Problem = ""
			t.Build = &info.Health.Build
			t.Workers = info.Health.Workers
			if status.Verified != nil {
				t.InterruptedWorkers = status.Verified.InterruptedWorkers
			}
		})
	case updateinstall.RolledBack, updateinstall.Failed:
		if err := c.record(run.ID, index, func(t *Target) { t.Grant = false }); err != nil {
			return err
		}
		return c.fail(run.ID, index, fmt.Errorf("%s: %s", status.Phase, status.Error))
	case updateinstall.RollbackFailed:
		return c.fail(run.ID, index, fmt.Errorf("%s: %s", status.Phase, status.Error))
	case updateinstall.Cancelled:
		return c.record(run.ID, index, func(t *Target) { t.State = Cancelled; t.Grant = false; t.Problem = "" })
	case updateinstall.Granted, updateinstall.Activating, updateinstall.Validating, updateinstall.RollingBack:
		return c.record(run.ID, index, func(t *Target) { t.State = Granted; t.Grant = true; t.Problem = status.Error })
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
	var status updateinstall.Status
	err := c.Remote.Call(ctx, target.Host, "install-cancel", Operation{ID: run.ID, Generation: target.Generation}, &status)
	if err != nil {
		var remote *RemoteError
		if errors.As(err, &remote) && remote.Problem == updateinstall.ErrAlreadyGranted.Error() {
			return c.record(run.ID, index, func(t *Target) {
				t.State, t.Grant = Granted, true
				t.Problem = "Activation was already authorized and may still finish."
			})
		}
		return c.recordError(run.ID, index, err)
	}
	if status.Phase != updateinstall.Cancelled || status.Request.ID != run.ID || status.Request.TargetID != target.Host.ID || status.Request.Generation != target.Generation || status.Request.Manifest.Digest() != run.ReleaseDigest {
		return c.fail(run.ID, index, errors.New("target did not acknowledge cancellation of the approved installation"))
	}
	return c.record(run.ID, index, func(t *Target) { t.State = Cancelled; t.Grant = false; t.Problem = "" })
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
		t.RetryAt = time.Now().Add(30 * time.Second)
	})
}
