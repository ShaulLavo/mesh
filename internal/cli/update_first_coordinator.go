package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

func previewFirstCoordinator(ctx context.Context, environment updateEnvironment, preview updatePreview) (updatePreview, error) {
	if preview.ClientOnly || environment.coordinator.ID != environment.local.ID {
		return preview, nil
	}
	var local update.Target
	for _, target := range preview.Targets {
		if target.Host.ID == environment.local.ID {
			local = target
			break
		}
	}
	added := local.Host.ID == ""
	if added {
		local = inspectUpdateTargets(ctx, environment.client, []update.Host{environment.local})[0]
	}
	if local.State == update.Offline && localDaemonAbsent(ctx, environment.stateDir) {
		preview.CoordinatorSetup = true
		local.State = update.Bootstrap
		local.Problem = "Approval installs a supervised local daemon and helper, preserves existing service configuration, and resumes this exact fleet operation."
		build := release.Current()
		local.Build = &build
		if executable, err := os.Executable(); err == nil {
			if migrationErr := coordinatorSetupMigration(executable, build, preview.Release); migrationErr != nil {
				local.State = update.Failed
				local.Problem = migrationErr.Error()
			}
		}
	}
	if local.State != update.Bootstrap && !preview.CoordinatorSetup {
		return preview, nil
	}
	preview.CoordinatorBootstrap, preview.CoordinatorAdded = true, added
	if added {
		preview.Fleet.Members = append(append([]update.Host(nil), preview.Fleet.Members...), environment.local)
		preview.Targets = append(preview.Targets, local)
	} else if preview.CoordinatorSetup {
		for index := range preview.Targets {
			if preview.Targets[index].Host.ID == local.Host.ID {
				preview.Targets[index] = local
			}
		}
	}
	return preview, preview.Fleet.Validate()
}

func (a *application) runFirstCoordinator(ctx context.Context, environment updateEnvironment, preview updatePreview, options updateOptions, output updateOutput) error {
	store, err := update.OpenStore(environment.stateDir)
	if err != nil {
		return err
	}
	run, err := store.Start(environment.local.ID, preview.Fleet, preview.Release)
	if err != nil {
		return err
	}
	run, request, err := approveFirstCoordinator(store, environment, run)
	if err != nil {
		return err
	}
	if preview.CoordinatorSetup {
		return a.startFirstCoordinatorSetup(ctx, environment, store, run, options, output)
	}
	return a.bootstrapApprovedCoordinator(ctx, environment, run, request, options, output)
}

func (a *application) bootstrapApprovedCoordinator(ctx context.Context, environment updateEnvironment, run update.Run, request updatebootstrap.Request, options updateOptions, output updateOutput) error {
	store, err := update.OpenStore(environment.stateDir)
	if err != nil {
		return err
	}
	cache, err := update.CacheDir()
	if err != nil {
		return err
	}
	bootstrap := a.dependencies.UpdateBootstrap
	if bootstrap == nil {
		bootstrap = updatebootstrap.Run
	}
	status, err := bootstrap(ctx, request, updatebootstrap.Config{
		StateDir: environment.stateDir, CacheDir: cache, RequiredMount: os.Getenv("MESH_UPDATE_REQUIRED_MOUNT"), Client: a.dependencies.UpdateRelease,
		Enroll: func(stateDir, coordinator string) error { return update.Trust(stateDir, coordinator, true) },
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if printErr := printUpdateRun(output.out, run, options.json); printErr != nil {
				return printErr
			}
			return statusError{code: 2}
		}
		run, recordErr := store.Change(run.ID, func(current *update.Run) error {
			current.Stopped, current.Problem = true, "Coordinator bootstrap failed: "+err.Error()
			return nil
		})
		if recordErr != nil {
			return recordErr
		}
		if printErr := printUpdateRun(output.out, run, options.json); printErr != nil {
			return printErr
		}
		return statusError{code: 1}
	}
	if status.Request.ID != request.ID || status.Request.TargetID != request.TargetID || status.Request.Generation != request.Generation || status.Request.Manifest.Digest() != request.Manifest.Digest() {
		return errors.New("coordinator bootstrap returned a different installation receipt")
	}
	if installationFinished(status.Phase) && status.Phase != updateinstall.Committed {
		run, err = recordCoordinatorReceipt(store, run, status)
		if err != nil {
			return err
		}
	}
	return observeUpdate(ctx, environment, run, options.json, output)
}

func approveFirstCoordinator(store *update.Store, environment updateEnvironment, run update.Run) (update.Run, updatebootstrap.Request, error) {
	index := coordinatorTargetIndex(run, environment.local.ID)
	if index < 0 {
		return run, updatebootstrap.Request{}, errors.New("coordinator bootstrap was not included in the approved scope")
	}
	generation := run.Targets[index].Generation
	if generation == 0 {
		var err error
		generation, err = firstCoordinatorGeneration(environment.stateDir, run.ID)
		if err != nil {
			return run, updatebootstrap.Request{}, err
		}
	}
	current, err := store.Change(run.ID, func(current *update.Run) error {
		if current.Cancel || current.Stopped {
			return errors.New("coordinator bootstrap operation is stopped; inspect its saved status before retrying")
		}
		current.CoordinatorBootstrap = true
		target := &current.Targets[index]
		target.Generation, target.Grant, target.State = generation, true, update.Granted
		return nil
	})
	request := updatebootstrap.Request{ID: run.ID, TargetID: environment.local.ID, CoordinatorID: environment.local.ID, Generation: generation, Manifest: run.Release}
	return current, request, err
}

func firstCoordinatorGeneration(stateDir, runID string) (uint64, error) {
	status, err := updateinstall.Read(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	if status.Request.ID == runID {
		return status.Request.Generation, nil
	}
	if !installationFinished(status.Phase) {
		return 0, fmt.Errorf("installation %s already owns the local coordinator", status.Request.ID)
	}
	if status.Request.Generation == ^uint64(0) {
		return 0, errors.New("installation generation exhausted")
	}
	return status.Request.Generation + 1, nil
}

func coordinatorTargetIndex(run update.Run, id string) int {
	for index, target := range run.Targets {
		if target.Host.ID == id {
			return index
		}
	}
	return -1
}

func readFirstCoordinatorRun(environment updateEnvironment, id string) (update.Run, error) {
	if environment.coordinator.ID != environment.local.ID {
		return update.Run{}, errors.New("coordinator is remote")
	}
	store, err := update.OpenStore(environment.stateDir)
	if err != nil {
		return update.Run{}, err
	}
	run, err := store.Read(id)
	if err != nil || !run.CoordinatorBootstrap {
		return update.Run{}, errors.New("no local coordinator bootstrap operation")
	}
	if run.CoordinatorSetup {
		return run, nil
	}
	status, err := updateinstall.Read(environment.stateDir)
	if err != nil {
		return run, nil
	}
	return recordCoordinatorReceipt(store, run, status)
}

func recordCoordinatorReceipt(store *update.Store, run update.Run, status updateinstall.Status) (update.Run, error) {
	index := coordinatorTargetIndex(run, run.Coordinator)
	if index < 0 || status.Request.ID != run.ID || status.Request.TargetID != run.Coordinator || status.Request.Generation != run.Targets[index].Generation || status.Request.Manifest.Digest() != run.ReleaseDigest {
		return run, errors.New("local coordinator receipt does not match approved operation")
	}
	if status.RetryToken < run.Targets[index].BootstrapRetryToken || !installationFinished(status.Phase) || status.Phase == updateinstall.Committed {
		return run, nil
	}
	return store.Change(run.ID, func(current *update.Run) error {
		current.Stopped = true
		target := &current.Targets[index]
		target.State, target.Problem = update.Failed, "Coordinator bootstrap "+string(status.Phase)+": "+status.Error
		if status.Phase != updateinstall.RollbackFailed {
			target.Grant = false
		}
		return nil
	})
}

func grantFirstCoordinator(ctx context.Context, engine *updateinstall.Engine, status updateinstall.Status) (updateinstall.Status, error) {
	if status.Settings.ClientOnly || status.Phase != updateinstall.Staged {
		return status, updateinstall.ErrNoGrant
	}
	local, err := identity.Load(status.Settings.StateDir)
	if err != nil {
		return status, err
	}
	store, err := update.OpenStore(status.Settings.StateDir)
	if err != nil {
		return status, err
	}
	run, err := store.Read(status.Request.ID)
	if errors.Is(err, os.ErrNotExist) {
		return status, updateinstall.ErrNoGrant
	}
	if err != nil {
		return status, err
	}
	index := coordinatorTargetIndex(run, local.ID)
	if !run.CoordinatorBootstrap || run.Coordinator != local.ID || run.Cancel || run.Stopped || index < 0 {
		return status, updateinstall.ErrNoGrant
	}
	target := run.Targets[index]
	if !target.Grant || target.Generation != status.Request.Generation || target.BootstrapRetryToken != status.RetryToken || status.Request.TargetID != local.ID || status.Request.Manifest.Digest() != run.ReleaseDigest {
		return status, updateinstall.ErrNoGrant
	}
	if err := update.Trust(status.Settings.StateDir, local.ID, true); err != nil {
		return status, err
	}
	return engine.Grant(ctx, status.Request.ID, status.Request.Generation)
}

func (a *application) operateFirstCoordinator(ctx context.Context, environment updateEnvironment, run update.Run, action string, structured bool, output updateOutput) error {
	store, err := update.OpenStore(environment.stateDir)
	if err != nil {
		return err
	}
	switch action {
	case "status":
	case "cancel":
		run, err = store.Cancel(run.ID)
	case "retry":
		return a.retryFirstCoordinator(ctx, environment, store, run, structured, output)
	default:
		return errors.New("unsupported coordinator bootstrap operation")
	}
	if err != nil {
		return err
	}
	if err := printUpdateRun(output.out, run, structured); err != nil {
		return err
	}
	return updateExit(run.ExitCode())
}

func (a *application) retryFirstCoordinator(ctx context.Context, environment updateEnvironment, store *update.Store, run update.Run, structured bool, output updateOutput) error {
	index := coordinatorTargetIndex(run, environment.local.ID)
	if index < 0 || run.Cancel {
		return errors.New("cancelled or invalid bootstrap operation requires a new approval")
	}
	if run.CoordinatorSetup {
		run, err := store.Change(run.ID, func(current *update.Run) error { current.Stopped = false; current.Problem = ""; return nil })
		if err != nil {
			return err
		}
		setup := a.dependencies.UpdateCoordinatorSetup
		if setup == nil {
			setup = installCoordinatorSetupHelper
		}
		if err = setup(ctx, environment.stateDir); err != nil {
			return err
		}
		return observeUpdate(ctx, environment, run, structured, output)
	}
	status, err := updateinstall.Read(environment.stateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	retry := err == nil && status.RetryToken >= run.Targets[index].BootstrapRetryToken && (status.Phase == updateinstall.RolledBack || status.Phase == updateinstall.Failed)
	if err == nil && status.Request.ID != run.ID {
		return errors.New("another installation owns this coordinator")
	}
	if err == nil && (status.Phase == updateinstall.RollbackFailed || status.Phase == updateinstall.Cancelled) {
		return errors.New("coordinator installation requires intervention or a new approval")
	}
	run, err = store.Change(run.ID, func(current *update.Run) error {
		if current.Cancel {
			return errors.New("cancelled operations require a new approval")
		}
		target := &current.Targets[index]
		if retry {
			if status.RetryToken == ^uint64(0) {
				return errors.New("bootstrap retry token exhausted")
			}
			target.BootstrapRetryToken = status.RetryToken + 1
		}
		target.State, target.Grant, target.Problem = update.Granted, true, ""
		current.Stopped, current.Problem = false, ""
		return nil
	})
	if err != nil {
		return err
	}
	target := run.Targets[index]
	request := updatebootstrap.Request{ID: run.ID, TargetID: environment.local.ID, CoordinatorID: environment.local.ID, Generation: target.Generation, RetryToken: target.BootstrapRetryToken, Manifest: run.Release}
	return a.bootstrapApprovedCoordinator(ctx, environment, run, request, updateOptions{json: structured}, output)
}

func resumeMissingCoordinatorInstallation(ctx context.Context, stateDir string) error {
	request, err := pendingFirstCoordinatorRequest(stateDir)
	if err != nil {
		return err
	}
	return resumeApprovedCoordinatorInstallation(ctx, stateDir, request)
}

func resumeApprovedCoordinatorInstallation(ctx context.Context, stateDir string, request updatebootstrap.Request) error {
	cache, err := update.CacheDir()
	if err != nil {
		return err
	}
	_, err = updatebootstrap.Run(ctx, request, updatebootstrap.Config{StateDir: stateDir, CacheDir: cache, RequiredMount: os.Getenv("MESH_UPDATE_REQUIRED_MOUNT"), Enroll: func(stateDir, coordinator string) error { return update.Trust(stateDir, coordinator, true) }})
	return err
}

func pendingFirstCoordinatorRequest(stateDir string) (updatebootstrap.Request, error) {
	local, err := identity.Load(stateDir)
	if err != nil {
		return updatebootstrap.Request{}, err
	}
	store, err := update.OpenStore(stateDir)
	if err != nil {
		return updatebootstrap.Request{}, err
	}
	runs, err := store.List()
	if err != nil {
		return updatebootstrap.Request{}, err
	}
	var request updatebootstrap.Request
	for _, run := range runs {
		index := coordinatorTargetIndex(run, local.ID)
		if !run.CoordinatorBootstrap || run.Coordinator != local.ID || run.Cancel || run.Stopped || index < 0 {
			continue
		}
		target := run.Targets[index]
		if !target.Grant || target.Generation == 0 {
			continue
		}
		if request.ID != "" {
			return request, errors.New("multiple approved bootstrap operations need local inspection")
		}
		request = updatebootstrap.Request{ID: run.ID, TargetID: local.ID, CoordinatorID: local.ID, Generation: target.Generation, RetryToken: target.BootstrapRetryToken, Manifest: run.Release}
	}
	if request.ID == "" {
		return request, os.ErrNotExist
	}
	return request, request.Validate()
}

func pendingCoordinatorRetry(status updateinstall.Status) (updatebootstrap.Request, bool) {
	if status.Settings.ClientOnly || (status.Phase != updateinstall.RolledBack && status.Phase != updateinstall.Failed) {
		return updatebootstrap.Request{}, false
	}
	request, err := pendingFirstCoordinatorRequest(status.Settings.StateDir)
	if err != nil || request.ID != status.Request.ID || request.TargetID != status.Request.TargetID || request.Generation != status.Request.Generation || request.Manifest.Digest() != status.Request.Manifest.Digest() || request.RetryToken <= status.RetryToken {
		return updatebootstrap.Request{}, false
	}
	return request, true
}
