package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
)

func (u *updateController) hostPolicy(ctx context.Context, actor update.VerifiedActor, action string, data json.RawMessage) (any, error) {
	if action == "host-policy.show" {
		policy, err := u.coordinator.Store.HostPolicy(ctx)
		return policy, hostControllerError(err)
	}
	_, remote := transport.Peer(ctx)
	if ctx.Value(localClientKey{}) != true || remote || actor.ID() != u.id {
		return nil, errors.New("update policy changes require this machine's owner through its local Unix socket")
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
