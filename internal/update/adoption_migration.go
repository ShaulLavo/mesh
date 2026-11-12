package update

import (
	"context"
	"errors"
	"reflect"

	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

func (u *AdoptionSupervisor) checkMigrationResume(ctx context.Context, operation Adoption) error {
	plan := operation.Migration
	if plan.Prepared && !plan.Applied {
		_, err := updatebootstrap.VerifyManagedMigrationSource(ctx, *plan, operation.Host.Health.Build.Digest)
		return hostError(err)
	}
	if !plan.Applied {
		actual, err := u.Service(ctx, operation.Settings)
		if err != nil || actual != operation.Host.Service {
			return errors.Join(errors.New("update: original package service changed before migration"), err)
		}
		return nil
	}
	actual, err := u.MigrationInspect(ctx, operation.Settings.StateDir, *plan)
	if err != nil {
		return hostError(err)
	}
	digest, err := adoptionServiceSnapshotDigest(actual.Service)
	if err != nil || actual.Settings != operation.Settings || actual.Observation.Executable != plan.Managed || digest != operation.Host.Service {
		return errors.Join(errors.New("update: managed daemon service changed during adoption"), err)
	}
	return nil
}

func (u *AdoptionSupervisor) migrateAdoption(ctx context.Context, operation Adoption, pinned AdoptionRelease) (bool, error) {
	plan := operation.Migration
	if plan == nil || plan.Applied && plan.GateCleared {
		return false, nil
	}
	if operation.Notice != nil && (operation.Notice.DeliveredAt.IsZero() || u.now().Before(operation.Notice.DeliveredAt.Add(operation.Notice.Lead))) {
		return true, u.hold(ctx, operation, errors.New("update: service migration waits for the notice and its lead time"))
	}
	pid, err := u.Helper(ctx, operation)
	if err != nil {
		return true, u.hold(ctx, operation, err)
	}
	if plan.Applied {
		return true, u.completeMigrationGate(ctx, operation, pinned)
	}
	if !plan.Prepared {
		return true, u.prepareMigration(ctx, operation, pinned, pid)
	}
	if plan.Health == nil {
		return true, u.stop(ctx, operation, AdoptionAttention, errors.New("update: prepared service migration has no retained worker snapshot"))
	}
	cfg := u.migrationConfig(operation, pinned, *plan.Health)
	if err := updateinstall.ApplyManagedAdoption(ctx, cfg); err != nil {
		return true, u.stop(ctx, operation, AdoptionAttention, err)
	}
	if err := u.sameHelper(ctx, operation, pid); err != nil {
		return true, u.stop(ctx, operation, AdoptionAttention, err)
	}
	next, migrated := operation, *plan
	migrated.Applied = true
	next.Migration, next.Settings = &migrated, migrated.Settings
	next.Host.Service, err = adoptionServiceSnapshotDigest(migrated.Next)
	if err != nil {
		return true, err
	}
	return true, u.Store.saveAdoption(ctx, operation, next)
}

func (u *AdoptionSupervisor) prepareMigration(ctx context.Context, operation Adoption, pinned AdoptionRelease, pid int) error {
	installation, err := u.Installation(ctx, operation.Settings.StateDir)
	if err != nil {
		return u.hold(ctx, operation, hostError(err))
	}
	health := installation.Observation.Health
	if err := pinned.verifySource(ctx, u.Documents, health); err != nil {
		return u.hold(ctx, operation, err)
	}
	content, err := updatebootstrap.ManagedMigrationContent(ctx, installation, *operation.Migration)
	if err != nil {
		return u.hold(ctx, operation, hostError(err))
	}
	cfg := u.migrationConfig(operation, pinned, health)
	cfg.Content = content
	if err := updateinstall.PrepareManagedAdoption(ctx, cfg); err != nil {
		return u.hold(ctx, operation, hostError(err))
	}
	if err := u.sameHelper(ctx, operation, pid); err != nil {
		return u.stop(ctx, operation, AdoptionAttention, err)
	}
	next, prepared := operation, *operation.Migration
	prepared.Prepared, prepared.Health = true, &health
	next.Migration = &prepared
	return u.Store.saveAdoption(ctx, operation, next)
}

func (u *AdoptionSupervisor) completeMigrationGate(ctx context.Context, operation Adoption, pinned AdoptionRelease) error {
	if operation.Migration.Health == nil {
		return errors.New("update: applied migration has no retained workers")
	}
	cfg := u.migrationConfig(operation, pinned, *operation.Migration.Health)
	if err := updateinstall.CompleteManagedAdoption(ctx, cfg); err != nil {
		return u.hold(ctx, operation, hostError(err))
	}
	next, migration := operation, *operation.Migration
	migration.GateCleared = true
	next.Migration = &migration
	return u.Store.saveAdoption(ctx, operation, next)
}

func (u *AdoptionSupervisor) migrationConfig(operation Adoption, pinned AdoptionRelease, health updateinstall.Health) updateinstall.AdoptionMigration {
	plan := *operation.Migration
	return updateinstall.AdoptionMigration{Operation: operation.ID, Pin: operation.Pin, Generation: operation.Request.Generation,
		Prior: plan.PriorSettings, Next: plan.Settings, Original: health, Definition: plan.Definition, Before: plan.Before, After: plan.After, ImageExisted: plan.ImageExists,
		NativeSource: func(ctx context.Context) (bool, error) {
			return updatebootstrap.VerifyManagedMigrationSource(ctx, plan, operation.Host.Health.Build.Digest)
		},
		Probe: func(ctx context.Context) (updateinstall.Health, error) {
			return u.migrationHealth(ctx, operation, false)
		},
		ManagedProbe: func(ctx context.Context) (updateinstall.Health, error) {
			return u.migrationHealth(ctx, operation, true)
		},
		Authorize: func(ctx context.Context, commit func() error) error {
			return u.authorizeMigration(ctx, operation, pinned, commit)
		}}
}

func (u *AdoptionSupervisor) migrationHealth(ctx context.Context, operation Adoption, managed bool) (updateinstall.Health, error) {
	plan := *operation.Migration
	var actual updatebootstrap.Installation
	var err error
	want, snapshot := plan.Original, plan.Prior
	if managed {
		actual, err = u.MigrationInspect(ctx, operation.Settings.StateDir, plan)
		want, snapshot = plan.Managed, plan.Next
	} else {
		actual, err = u.Installation(ctx, operation.Settings.StateDir)
	}
	if err != nil {
		return updateinstall.Health{}, hostError(err)
	}
	if actual.Observation.Executable != want || !reflect.DeepEqual(actual.Service, snapshot) {
		return actual.Observation.Health, errors.New("update: migrated service did not preserve its exact invocation and settings")
	}
	return actual.Observation.Health, nil
}

func (u *AdoptionSupervisor) authorizeMigration(ctx context.Context, operation Adoption, pinned AdoptionRelease, commit func() error) error {
	unlock, err := u.Store.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	envelope, actual, err := u.Store.adoption()
	if err != nil {
		return err
	}
	if envelope.Confirmed || actual.Phase != AdoptionActivating || actual.Migration == nil || !reflect.DeepEqual(actual, operation) || actual.Pin != pinned.Pin || actual.Signature != pinned.Signature {
		return errors.New("update: service migration lacks its exact canonical adoption approval")
	}
	policy, err := u.Store.hostPolicy()
	if err != nil || !reflect.DeepEqual(policy, actual.Policy) {
		return errors.Join(errors.New("update: migration host policy changed"), err)
	}
	if err := checkAdoptionIdentity(operation.Settings.StateDir, operation.Host.Health.HostID); err != nil {
		return err
	}
	if err := checkAdoptionFiles(operation.Host.Preserved); err != nil {
		return err
	}
	if err := u.checkMigrationResume(ctx, operation); err != nil {
		return err
	}
	if operation.Notice != nil && (operation.Notice.DeliveredAt.IsZero() || u.now().Before(operation.Notice.DeliveredAt.Add(operation.Notice.Lead))) {
		return errors.New("update: service migration notice has not completed")
	}
	return commit()
}

func (u *AdoptionSupervisor) adoptionConfirmationService(operation Adoption) func(context.Context, updateinstall.Settings) (string, error) {
	if operation.Migration == nil {
		return u.Service
	}
	return func(ctx context.Context, _ updateinstall.Settings) (string, error) {
		actual, err := u.MigrationInspect(ctx, operation.Settings.StateDir, *operation.Migration)
		if err != nil {
			return "", hostError(err)
		}
		return adoptionServiceSnapshotDigest(actual.Service)
	}
}
