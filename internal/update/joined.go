package update

import (
	"context"
	"errors"

	"github.com/shaul/mesh/internal/updateinstall"
)

func joinableInstallation(status *updateinstall.Status, digest, targetID string) bool {
	if status == nil || status.Request.Manifest.Digest() != digest || status.Request.TargetID != targetID {
		return false
	}
	switch status.Phase {
	case updateinstall.Accepted, updateinstall.Staged, updateinstall.Granted, updateinstall.Activating, updateinstall.Validating, updateinstall.RollingBack:
		return true
	default:
		return false
	}
}

func (c *Coordinator) joinInstallation(ctx context.Context, run Run, index int, info Info) error {
	status := info.Installation
	if !joinableInstallation(status, run.ReleaseDigest, run.Targets[index].Host.ID) {
		return c.fail(run.ID, index, errors.New("target returned another operation's installation"))
	}
	current, err := c.Store.Change(run.ID, func(r *Run) error {
		target := &r.Targets[index]
		target.InstallationID = status.Request.ID
		target.Generation = status.Request.Generation
		target.Build, target.Workers = &info.Health.Build, info.Health.Workers
		return nil
	})
	if err != nil {
		return err
	}
	return c.reconcileInstallation(ctx, current, index, info)
}

func (c *Coordinator) observeJoinedStaging(run Run, index int, phase updateinstall.Phase) error {
	return c.record(run.ID, index, func(target *Target) {
		target.Grant = false
		target.State = Pending
		if phase == updateinstall.Staged {
			target.State = Staged
		}
		target.Problem = "Waiting for the original operation " + target.InstallationID + "."
		if run.Cancel {
			target.State = Cancelled
			target.Problem = "This operation stopped waiting; the original operation may still update this host."
		}
	})
}
