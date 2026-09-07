package updateinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updategate"
)

func (e *Engine) Run(ctx context.Context) (Status, error) {
	lock, err := lockInstallation(ctx, e.cfg.StateDir)
	if err != nil {
		return Status{}, err
	}
	defer unlock(lock)
	status, err := e.Read()
	if err != nil {
		return status, err
	}
	if status.Phase == Accepted {
		status, err = e.stage(ctx, status)
	}
	if err != nil {
		return status, err
	}
	if status.Phase == Staged {
		return status, ErrNoGrant
	}
	if status.Phase == Granted || status.Phase == Activating {
		return e.activate(ctx, status)
	}
	if status.Phase == Validating {
		return e.validate(ctx, status)
	}
	if status.Phase == RollingBack {
		return e.rollback(status)
	}
	if status.Phase == RollbackFailed || status.Phase == Failed {
		return status, errors.New(status.Error)
	}
	if status.Phase == Committed || status.Phase == RolledBack {
		return status, updategate.Clear(e.cfg.StateDir, status.Request.ID)
	}
	return status, nil
}

func (e *Engine) activate(ctx context.Context, status Status) (Status, error) {
	if err := checkMount(e.cfg.RequiredMount); err != nil {
		return status, err
	}
	if status.Phase == Granted {
		if err := verifyFile(e.cfg.Executable, status.Request.Current.Digest); err != nil {
			return e.failBeforeActivation(status, err)
		}
	}
	if err := updategate.Set(e.cfg.StateDir, status.Request.ID); err != nil {
		return status, err
	}
	if status.Phase == Granted && !e.cfg.ClientOnly {
		baseline, err := e.awaitBaseline(ctx)
		if err != nil {
			status.Error = "waiting for the original daemon before activation: " + err.Error()
			return status, errors.Join(errors.New(status.Error), e.save(&status), updategate.Clear(e.cfg.StateDir, status.Request.ID))
		}
		if baseline.HostID != status.Request.TargetID || baseline.Build.Digest != status.Request.Current.Digest {
			return e.failBeforeActivation(status, errors.New("running installation changed after staging"))
		}
		if err = compatibleWorkers(baseline, status.Request.Manifest.Compatibility); err != nil {
			return e.failBeforeActivation(status, err)
		}
		baseline.InterruptedWorkers = interruptedWorkers(status.Original, baseline)
		status.Original = baseline
	}
	status.Phase = Activating
	if err := e.save(&status); err != nil {
		return status, err
	}
	if err := e.stop(ctx); err != nil {
		return e.beginRollback(status, err)
	}
	if err := e.switchCandidate(status); err != nil {
		if errors.Is(err, ErrInstallationChanged) {
			return e.failBeforeActivation(status, err)
		}
		return e.beginRollback(status, err)
	}
	status.Phase = Validating
	if err := e.save(&status); err != nil {
		return status, err
	}
	return e.validate(ctx, status)
}

func (e *Engine) awaitBaseline(ctx context.Context) (Health, error) {
	ctx, cancel := context.WithTimeout(ctx, e.cfg.HealthTimeout)
	defer cancel()
	health, err := e.cfg.Probe(ctx)
	if err == nil {
		return health, nil
	}
	if err = e.start(ctx); err != nil {
		return Health{}, err
	}
	for {
		health, err = e.cfg.Probe(ctx)
		if err == nil {
			return health, nil
		}
		if waitErr := waitContext(ctx, 100*time.Millisecond); waitErr != nil {
			return Health{}, errors.Join(err, waitErr)
		}
	}
}

func (e *Engine) switchCandidate(status Status) error {
	artifact, err := status.Request.Manifest.Artifact(status.Request.Current.Platform)
	if err != nil {
		return err
	}
	digest, err := fileDigest(e.cfg.Executable)
	if err != nil {
		return err
	}
	if digest == artifact.BinarySHA256 {
		return nil
	}
	if digest != status.Request.Current.Digest {
		return ErrInstallationChanged
	}
	if err = durableLink(e.cfg.Executable, status.Previous, status.Request.Current.Digest); err != nil {
		return err
	}
	if err = verifyFile(status.Previous, status.Request.Current.Digest); err != nil {
		return err
	}
	if err = verifyFile(status.Candidate, artifact.BinarySHA256); err != nil {
		return err
	}
	if err = os.Rename(status.Candidate, e.cfg.Executable); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(e.cfg.Executable))
}

func (e *Engine) failBeforeActivation(status Status, cause error) (Status, error) {
	status.Phase, status.Error = Failed, cause.Error()
	if err := e.save(&status); err != nil {
		return status, errors.Join(cause, err)
	}
	return status, errors.Join(cause, updategate.Clear(e.cfg.StateDir, status.Request.ID))
}

func (e *Engine) validate(ctx context.Context, status Status) (Status, error) {
	if err := updategate.Set(e.cfg.StateDir, status.Request.ID); err != nil {
		return status, err
	}
	if err := e.start(ctx); err != nil {
		return e.beginRollback(status, err)
	}
	artifact, err := status.Request.Manifest.Artifact(status.Request.Current.Platform)
	if err != nil {
		return e.beginRollback(status, err)
	}
	expected := release.Build{Version: status.Request.Manifest.Version, Commit: status.Request.Manifest.Commit,
		Digest: artifact.BinarySHA256, Platform: status.Request.Current.Platform}
	health, err := e.awaitHealth(ctx, expected, status)
	if err != nil {
		return e.beginRollback(status, err)
	}
	status.Phase, status.Verified, status.Error = Committed, &health, ""
	if err = e.save(&status); err != nil {
		return status, err
	}
	return status, updategate.Clear(e.cfg.StateDir, status.Request.ID)
}

func (e *Engine) beginRollback(status Status, cause error) (Status, error) {
	status.Phase, status.Error = RollingBack, cause.Error()
	if err := e.save(&status); err != nil {
		return status, errors.Join(cause, err)
	}
	return e.rollback(status)
}

func (e *Engine) rollback(status Status) (Status, error) {
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.HealthTimeout+15*time.Second)
	defer cancel()
	health, err := e.restore(ctx, status)
	if err != nil {
		status.Phase, status.Error = RollbackFailed, status.Error+"; rollback failed: "+err.Error()
		return status, errors.Join(errors.New(status.Error), e.save(&status))
	}
	status.Phase, status.Verified = RolledBack, &health
	if err = e.save(&status); err != nil {
		return status, err
	}
	if err = updategate.Clear(e.cfg.StateDir, status.Request.ID); err != nil {
		return status, err
	}
	return status, fmt.Errorf("update rolled back: %s", status.Error)
}

func (e *Engine) restore(ctx context.Context, status Status) (Health, error) {
	if err := updategate.Set(e.cfg.StateDir, status.Request.ID); err != nil {
		return Health{}, err
	}
	if err := e.stop(ctx); err != nil {
		return Health{}, err
	}
	if err := durableLink(status.Previous, e.cfg.Executable, status.Request.Current.Digest); err != nil {
		return Health{}, err
	}
	if err := e.start(ctx); err != nil {
		return Health{}, err
	}
	return e.awaitHealth(ctx, status.Request.Current, status)
}

func (e *Engine) stop(ctx context.Context) error {
	if e.cfg.ClientOnly {
		return nil
	}
	return e.cfg.Service.Stop(ctx)
}

func (e *Engine) start(ctx context.Context) error {
	if e.cfg.ClientOnly {
		return nil
	}
	return e.cfg.Service.Start(ctx)
}

func (e *Engine) awaitHealth(ctx context.Context, expected release.Build, status Status) (Health, error) {
	ctx, cancel := context.WithTimeout(ctx, e.cfg.HealthTimeout)
	defer cancel()
	var last error
	for {
		health, err := e.probe(ctx, status.Request.TargetID)
		if err == nil {
			err = verifyHealth(health, expected, status)
		}
		if err == nil {
			health.InterruptedWorkers = interruptedWorkers(status.Original, health)
			return health, nil
		}
		last = err
		if err = waitContext(ctx, 100*time.Millisecond); err != nil {
			return Health{}, fmt.Errorf("executing-build health deadline: %w", errors.Join(last, err))
		}
	}
}

func (e *Engine) probe(ctx context.Context, targetID string) (Health, error) {
	if !e.cfg.ClientOnly {
		return e.cfg.Probe(ctx)
	}
	command := exec.CommandContext(ctx, e.cfg.Executable, "version", "--json") //nolint:gosec // verified executable selected by the durable installation transaction
	output, err := command.Output()
	if err != nil {
		return Health{}, fmt.Errorf("verify installed client: %w", err)
	}
	if len(output) > 64<<10 {
		return Health{}, errors.New("installed client build report exceeds size limit")
	}
	var build release.Build
	if err = json.Unmarshal(output, &build); err != nil {
		return Health{}, err
	}
	return Health{HostID: targetID, Build: build}, nil
}

func verifyHealth(health Health, expected release.Build, status Status) error {
	if health.HostID != status.Request.TargetID {
		return errors.New("running daemon identity mismatch")
	}
	if health.Build.Digest != expected.Digest || health.Build.Version != expected.Version || health.Build.Commit != expected.Commit || health.Build.Platform != expected.Platform {
		return errors.New("executing build does not match the approved executable")
	}
	compatibility := status.Request.Manifest.Compatibility
	if health.Build.StateVersion < compatibility.StateReadMin || health.Build.StateVersion > compatibility.StateReadMax {
		return errors.New("actual database schema is outside the tested compatibility range")
	}
	if expected.Digest != status.Request.Current.Digest && health.Build.StateVersion != compatibility.StateWrite {
		return errors.New("candidate did not finish the expected database migration")
	}
	if changedBoot(status.Original, health) {
		return nil
	}
	workers := make(map[string]Worker, len(health.Workers))
	for _, worker := range health.Workers {
		workers[worker.ID] = worker
	}
	for _, previous := range status.Original.Workers {
		current, exists := workers[previous.ID]
		if !exists || current.PID != previous.PID || current.ShellPID != previous.ShellPID {
			return fmt.Errorf("session %s worker was lost or replaced during activation", previous.ID)
		}
	}
	return nil
}

func changedBoot(previous, current Health) bool {
	return previous.BootID != "" && current.BootID != "" && previous.BootID != current.BootID
}

func interruptedWorkers(previous, current Health) []Worker {
	interrupted := append([]Worker(nil), previous.InterruptedWorkers...)
	if changedBoot(previous, current) {
		interrupted = append(interrupted, previous.Workers...)
	}
	return interrupted
}
