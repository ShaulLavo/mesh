package update

import (
	"errors"

	"github.com/shaul/mesh/internal/updateinstall"
)

// Only a matching receipt can settle an issued intention. An absent journal
// cannot fence a request that is still in flight.
func (c *Coordinator) observeAutomaticDispatch(run Run, index int, status updateinstall.Status) (Run, error) {
	target := run.Targets[index]
	if run.Origin == nil || target.InstallationID != "" || !target.StageUnsettled && !target.RetryUnsettled {
		return run, nil
	}
	return c.Store.Change(run.ID, func(current *Run) error {
		target := &current.Targets[index]
		if !matchesAutomaticDispatch(*current, *target, status) {
			return errors.New("update: installation receipt differs from the issued automatic intention")
		}
		settledRetry := target.RetryUnsettled && status.RetryToken >= target.RetryToken
		if !target.StageUnsettled && !settledRetry {
			return nil
		}
		target.StageUnsettled = false
		if settledRetry {
			target.RetryUnsettled = false
		}
		// Do not expose a prior Failed/Cancelled state between accepting a
		// nonterminal receipt and reconciling its actual phase.
		target.State = Pending
		return nil
	})
}

func matchesAutomaticDispatch(run Run, target Target, status updateinstall.Status) bool {
	request := status.Request
	return request.ID == run.ID && request.TargetID == target.Host.ID && request.Generation == target.Generation && request.Manifest.Digest() == run.ReleaseDigest && sameInstallationOrigin(request.Automatic, automaticRequest(run, target))
}
