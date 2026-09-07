package update

import (
	"context"
	"errors"

	"github.com/shaul/mesh/internal/updateinstall"
)

func (c *Coordinator) reconcileRetry(ctx context.Context, run Run, index int, info Info) error {
	target := run.Targets[index]
	status := *info.Installation
	if !run.Cancel && status.RetryToken < target.RetryToken && (status.Phase == updateinstall.RolledBack || status.Phase == updateinstall.Failed) {
		var receipt updateinstall.Status
		err := c.Remote.Call(ctx, target.Host, "install-retry", Operation{ID: run.ID, Generation: target.Generation, RetryToken: target.RetryToken}, &receipt)
		if err != nil {
			return c.recordError(run.ID, index, err)
		}
		if receipt.Request.ID != run.ID || receipt.Request.TargetID != target.Host.ID || receipt.Request.Generation != target.Generation || receipt.Request.Manifest.Digest() != run.ReleaseDigest || receipt.RetryToken != target.RetryToken {
			return c.fail(run.ID, index, errors.New("retry receipt differs from the approved installation and retry"))
		}
		return c.record(run.ID, index, func(target *Target) { target.RetryPending = false })
	}
	current, err := c.Store.Change(run.ID, func(current *Run) error {
		current.Targets[index].RetryPending = false
		return nil
	})
	if err != nil {
		return err
	}
	return c.reconcileInstallation(ctx, current, index, info)
}
