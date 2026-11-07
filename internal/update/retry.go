package update

import (
	"context"
	"errors"

	"github.com/shaul/mesh/internal/updateinstall"
)

func (c *Coordinator) reconcileRetry(ctx context.Context, run Run, index int, info Info) error {
	target := run.Targets[index]
	status := *info.Installation
	if !needsInstallationRetry(run, target, status) {
		return c.finishRetryObservation(ctx, run, index, info)
	}

	current, issued, err := c.reserveRetry(ctx, run, index)
	if stoppedAutomaticError(err) {
		return nil
	}
	if err != nil || !issued {
		return err
	}
	run = current
	target = run.Targets[index]

	var receipt updateinstall.Status
	err = c.Remote.Call(ctx, target.Host, "install-retry", Operation{ID: run.ID, Generation: target.Generation, RetryToken: target.RetryToken}, &receipt)
	if err != nil {
		return c.recordError(run.ID, index, err)
	}
	if receipt.Request.ID != run.ID || receipt.Request.TargetID != target.Host.ID || receipt.Request.Generation != target.Generation || receipt.Request.Manifest.Digest() != run.ReleaseDigest || receipt.RetryToken != target.RetryToken {
		return c.fail(run.ID, index, errors.New("retry receipt differs from the approved installation and retry"))
	}
	return c.record(run.ID, index, func(target *Target) { target.RetryPending = false })
}
func needsInstallationRetry(run Run, target Target, status updateinstall.Status) bool {
	return !run.Cancel && status.RetryToken < target.RetryToken && (status.Phase == updateinstall.RolledBack || status.Phase == updateinstall.Failed)
}
func (c *Coordinator) finishRetryObservation(ctx context.Context, run Run, index int, info Info) error {

	current, err := c.Store.Change(run.ID, func(current *Run) error {
		current.Targets[index].RetryPending = false
		return nil
	})
	if err != nil {
		return err
	}
	return c.reconcileInstallation(ctx, current, index, info)
}

func (c *Coordinator) reserveRetry(ctx context.Context, run Run, index int) (Run, bool, error) {
	if run.Origin == nil {
		return run, true, nil
	}
	return c.reserveAutomatic(ctx, run.ID, index, automaticRetry)
}
