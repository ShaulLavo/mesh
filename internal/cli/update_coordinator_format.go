package cli

import (
	"context"
	"fmt"

	"github.com/shaul/mesh/internal/update"
)

func requireIdentityFleetCoordinator(ctx context.Context, environment updateEnvironment) error {
	query, cancel := context.WithTimeout(ctx, remoteConnectTimeout)
	defer cancel()
	var info update.Info
	err := environment.client.Call(query, environment.coordinator, "info", nil, &info)
	if err != nil {
		if environment.coordinator.ID == environment.local.ID && (legacyUpdateUnavailable(err) || localDaemonAbsent(query, environment.stateDir)) {
			return nil
		}
		return fmt.Errorf("inspect update coordinator %s: %w", environment.coordinator.Label(), err)
	}
	if info.Health.HostID != environment.coordinator.ID {
		return fmt.Errorf("update coordinator identity differs from its pinned identity")
	}
	if info.AcceptsIdentityFleet {
		return nil
	}
	version := info.Health.Build.Version
	if version == "" {
		version = "unknown version"
	}
	if environment.coordinator.ID == environment.local.ID {
		return fmt.Errorf("this machine's daemon runs older Mesh %s; run mesh daemon install to restart it with this client, then retry mesh update --local", version)
	}
	return fmt.Errorf("machine %s runs older Mesh %s; run mesh update --local on that machine first, then retry with this coordinator", environment.coordinator.Label(), version)
}
