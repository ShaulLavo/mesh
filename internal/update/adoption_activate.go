package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
)

func (u *AdoptionSupervisor) activate(ctx context.Context, operation Adoption, pinned AdoptionRelease) error {
	if u.Caller == nil || u.Helper == nil || u.Engine == nil {
		return u.hold(ctx, operation, errors.New("update: adoption activation boundaries unavailable"))
	}
	if err := u.checkPeers(ctx, operation); err != nil {
		return u.hold(ctx, operation, err)
	}
	if handled, err := u.migrateAdoption(ctx, operation, pinned); handled || err != nil {
		return err
	}
	pid, err := u.Helper(ctx, operation)
	if err != nil {
		return u.hold(ctx, operation, err)
	}
	status, err := updateinstall.ReadContext(ctx, operation.Settings.StateDir)
	if err != nil && !(operation.Unjournaled && errors.Is(err, os.ErrNotExist)) {
		return u.hold(ctx, operation, err)
	}
	if !sameAdoptionTransaction(operation, status) {
		status, err = u.stageAdoption(ctx, operation, status)
		if err != nil {
			return u.hold(ctx, operation, err)
		}
	}
	if err := checkAdoptionTransaction(operation, status); err != nil {
		return u.stop(ctx, operation, AdoptionAttention, err)
	}
	if err := u.sameHelper(ctx, operation, pid); err != nil {
		return u.stop(ctx, operation, AdoptionAttention, err)
	}
	return u.runAdoption(ctx, operation, status, pid)
}

func sameAdoptionTransaction(operation Adoption, status updateinstall.Status) bool {
	return status.Request.ID == operation.ID && status.Request.Generation == operation.Request.Generation
}

func checkAdoptionTransaction(operation Adoption, status updateinstall.Status) error {
	if !reflect.DeepEqual(status.Request, operation.Request) || status.Settings != operation.Settings {
		return errors.New("update: adoption journal changed request, pin, or preserved settings")
	}
	return nil
}

func (u *AdoptionSupervisor) stageAdoption(ctx context.Context, operation Adoption, prior updateinstall.Status) (updateinstall.Status, error) {
	initial := operation.Unjournaled && prior.Phase == "" && prior.Request.ID == "" && operation.Request.Generation == 1
	settingsMatch := prior.Settings == operation.Settings || operation.Migration != nil && prior.Settings == operation.Migration.PriorSettings
	if !initial && (!hostFinished(prior.Phase) || prior.Request.Generation >= operation.Request.Generation || !settingsMatch) {
		return prior, errors.New("update: another transaction owns the adoption installation")
	}
	local := LocalHost(operation.Settings.StateDir, operation.Host.Health.HostID)
	var info Info
	if err := u.Caller.Call(ctx, local, "info", nil, &info); err != nil {
		return prior, fmt.Errorf("update: local adoption info refused: %w", err)
	}
	if err := checkAdoptionHealth(operation, info.Health, false); err != nil {
		return prior, err
	}
	pinned, err := u.verifyResume(ctx, operation)
	if err != nil {
		return prior, err
	}
	if err := pinned.verifySource(ctx, u.Documents, info.Health); err != nil {
		return prior, err
	}
	var status updateinstall.Status
	if err := u.Caller.Call(ctx, local, "stage", operation.Request, &status); err != nil {
		return status, fmt.Errorf("update: local adoption stage refused: %w", err)
	}
	return status, checkAdoptionTransaction(operation, status)
}

func (u *AdoptionSupervisor) sameHelper(ctx context.Context, operation Adoption, pid int) error {
	actual, err := u.Helper(ctx, operation)
	if err != nil {
		return err
	}
	if actual != pid || pid <= 0 {
		return errors.New("update: promoted helper process changed during adoption control")
	}
	return nil
}

func (u *AdoptionSupervisor) runAdoption(ctx context.Context, operation Adoption, status updateinstall.Status, pid int) error {
	if status.Phase == updateinstall.Accepted {
		local := LocalHost(operation.Settings.StateDir, operation.Host.Health.HostID)
		if err := u.Caller.Call(ctx, local, "stage", operation.Request, &status); err != nil {
			return u.hold(ctx, operation, fmt.Errorf("update: local adoption staging refused: %w", err))
		}
	}
	if status.Phase == updateinstall.Staged {
		if err := u.grantAdoption(ctx, operation, &status); err != nil {
			return u.hold(ctx, operation, err)
		}
	}
	if err := checkAdoptionTransaction(operation, status); err != nil {
		return u.stop(ctx, operation, AdoptionAttention, err)
	}
	engine, err := u.Engine(operation)
	if err != nil {
		return u.hold(ctx, operation, err)
	}
	status, runErr := engine.Run(ctx)
	if err := u.sameHelper(ctx, operation, pid); err != nil {
		return u.stop(ctx, operation, AdoptionAttention, errors.Join(runErr, err))
	}
	return u.afterRun(ctx, operation, status, runErr)
}

func (u *AdoptionSupervisor) grantAdoption(ctx context.Context, operation Adoption, status *updateinstall.Status) error {
	if err := checkAdoptionTransaction(operation, *status); err != nil {
		return err
	}
	if err := u.checkPeers(ctx, operation); err != nil {
		return err
	}
	if operation.Notice != nil && (operation.Notice.DeliveredAt.IsZero() || u.now().Before(operation.Notice.DeliveredAt.Add(operation.Notice.Lead))) {
		return errors.New("update: adoption notice and lead time must complete before grant")
	}
	local := LocalHost(operation.Settings.StateDir, operation.Host.Health.HostID)
	if err := u.Caller.Call(ctx, local, "grant", Operation{ID: operation.ID, Generation: operation.Request.Generation}, status); err != nil {
		return fmt.Errorf("update: local adoption grant refused: %w", err)
	}
	return nil
}

func (u *AdoptionSupervisor) afterRun(ctx context.Context, operation Adoption, status updateinstall.Status, runErr error) error {
	switch status.Phase {
	case updateinstall.Committed:
		next := operation
		next.Phase, next.Problem, next.NextAttempt = AdoptionConfirming, "", time.Time{}
		return u.Store.saveAdoption(ctx, operation, next)
	case updateinstall.RolledBack:
		return u.stop(ctx, operation, AdoptionRolledBack, errors.Join(runErr, errors.New("update: pinned adoption rolled back; descendants remain held")))
	case updateinstall.RollbackFailed, updateinstall.Failed, updateinstall.Cancelled:
		return u.stop(ctx, operation, AdoptionAttention, errors.Join(runErr, errors.New("update: pinned adoption requires attention; descendants remain held")))
	case updateinstall.Accepted, updateinstall.Staged, updateinstall.Granted, updateinstall.Activating, updateinstall.Validating, updateinstall.RollingBack:
		return runErr
	default:
		return u.stop(ctx, operation, AdoptionAttention, errors.New("update: adoption installation returned an unknown phase"))
	}
}

func (u *AdoptionSupervisor) stop(ctx context.Context, operation Adoption, phase AdoptionPhase, cause error) error {
	next := operation
	next.Phase, next.Problem = phase, boundedProblem(cause.Error())
	return errors.Join(cause, u.Store.saveAdoption(ctx, operation, next))
}

func (u *AdoptionSupervisor) confirm(ctx context.Context, operation Adoption) error {
	status, err := updateinstall.ReadContext(ctx, operation.Settings.StateDir)
	if err != nil {
		return u.hold(ctx, operation, err)
	}
	if _, err := u.Helper(ctx, operation); err != nil {
		return u.hold(ctx, operation, err)
	}
	probe := func(ctx context.Context) (updateinstall.Health, error) {
		observed, err := u.Inspect(ctx, operation.Settings.StateDir)
		if err == nil && observed.Executable != operation.Settings.Executable {
			err = errors.New("update: adopted daemon runs from a different installation path")
		}
		return observed.Health, err
	}
	return u.Store.confirmAdoption(ctx, operation, status, probe, u.adoptionConfirmationService(operation))
}
