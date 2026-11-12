package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
)

func (u *updateController) requireLocalUpdateOwner(ctx context.Context, actor update.VerifiedActor) error {
	_, remote := transport.Peer(ctx)
	if ctx.Value(localClientKey{}) != true || remote || actor.ID() != u.id {
		return errors.New("update changes require this machine's owner through its local Unix socket")
	}
	return nil
}

func (u *updateController) localTarget(ctx context.Context, actor update.VerifiedActor, action string, data json.RawMessage) (any, error) {
	if err := u.requireLocalUpdateOwner(ctx, actor); err != nil {
		return nil, err
	}
	if action == update.HostTargetStatusAction {
		status, err := u.coordinator.Store.HostTarget(ctx)
		return status, hostControllerError(err)
	}
	if action == update.HostTargetCancelAction {
		var expected uint64
		if err := update.DecodeAutomatic(data, &expected); err != nil {
			return nil, fmt.Errorf("read local target revision: %w", err)
		}
		status, err := u.coordinator.Store.CancelLocalTarget(ctx, expected)
		return status, hostControllerError(err)
	}
	if u.hostUpdater == nil {
		return nil, errors.New("local host updater unavailable")
	}
	var input update.HostTargetSubmission
	if err := update.DecodeAutomatic(data, &input); err != nil {
		return nil, fmt.Errorf("read local update target: %w", err)
	}
	if action == update.HostTargetPlanAction {
		plan, err := u.hostUpdater.PlanLocalTarget(ctx, input)
		return plan, hostControllerError(err)
	}
	status, err := u.hostUpdater.SubmitLocalTarget(ctx, input)
	return status, hostControllerError(err)
}
