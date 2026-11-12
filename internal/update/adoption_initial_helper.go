package update

import (
	"context"
	"errors"
	"reflect"
)

func (u *AdoptionSupervisor) authorizeInitialHelper(ctx context.Context, operation Adoption, pinned AdoptionRelease, commit func() error) error {
	unlock, err := u.Store.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	envelope, actual, err := u.Store.adoption()
	if err != nil {
		return err
	}
	if envelope.Confirmed || !actual.Unjournaled || actual.Phase != AdoptionRecorded || !reflect.DeepEqual(actual, operation) || envelope.Pin != pinned.Pin || operation.Signature != pinned.Signature {
		return errors.New("update: initial helper lacks its exact unconfirmed canonical adoption approval")
	}
	policy, err := u.Store.hostPolicy()
	if err != nil || !reflect.DeepEqual(policy, actual.Policy) {
		return errors.Join(errors.New("update: initial adoption policy changed"), err)
	}
	if err := u.checkInitialHelperSource(ctx, operation, pinned); err != nil {
		return err
	}
	return commit()
}

func (u *AdoptionSupervisor) checkInitialHelperSource(ctx context.Context, operation Adoption, pinned AdoptionRelease) error {
	installation, err := u.Installation(ctx, operation.Settings.StateDir)
	if err != nil {
		return hostError(err)
	}
	if installation.Journal || installation.Settings != operation.Settings || !sameHostIdentity(installation.Observation.Health.Build, operation.Host.Health.Build) {
		return errors.New("update: initial helper daemon/service settings or source changed")
	}
	if err := checkAdoptionIdentity(operation.Settings.StateDir, operation.Host.Health.HostID); err != nil {
		return err
	}
	if err := checkAdoptionHealth(operation, installation.Observation.Health, false); err != nil {
		return err
	}
	if err := checkAdoptionFiles(operation.Host.Preserved); err != nil {
		return err
	}
	service, err := adoptionServiceSnapshotDigest(installation.Service)
	if err != nil || service != operation.Host.Service {
		return errors.Join(errors.New("update: initial helper's actual daemon service changed"), err)
	}
	for _, worker := range installation.Observation.Health.Workers {
		if err := verifyHostWorker(worker, pinned.Manifest.Compatibility, pinned.ProvedWorkers); err != nil {
			return err
		}
	}
	return nil
}
