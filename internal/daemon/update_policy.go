package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/shaul/mesh/internal/update"
)

func (u *updateController) hostPolicy(ctx context.Context, actor update.VerifiedActor, action string, data json.RawMessage) (any, error) {
	if action == "host-policy.show" {
		policy, err := u.coordinator.Store.HostPolicy(ctx)
		return policy, hostControllerError(err)
	}
	if err := u.requireLocalUpdateOwner(ctx, actor); err != nil {
		return nil, err
	}
	if action == "host-policy.forget" {
		var dependent update.HostDependentForget
		if err := update.DecodeAutomatic(data, &dependent); err != nil {
			return nil, fmt.Errorf("read dependent to forget: %w", err)
		}
		err := u.coordinator.Store.ForgetHostDependent(ctx, dependent.ID)
		return dependent, hostControllerError(err)
	}
	var change update.HostPolicyChange
	if err := update.DecodeAutomatic(data, &change); err != nil {
		return nil, fmt.Errorf("read host policy change: %w", err)
	}
	policy, err := u.coordinator.Store.SetHostPolicy(ctx, change.ExpectedRevision, change.Policy)
	if err != nil {
		return nil, fmt.Errorf("change host policy: %w", err)
	}
	return policy, nil
}
