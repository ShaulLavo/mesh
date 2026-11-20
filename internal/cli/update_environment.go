package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/update"
)

func existingAutomaticEnvironment(ctx context.Context, coordinator string) (updateEnvironment, error) {
	stateDir, err := paths.StateDirPath()
	if err != nil {
		return updateEnvironment{}, fmt.Errorf("automatic state directory: %w", err)
	}
	host, key, err := identity.LoadPrivate(stateDir)
	if err != nil {
		return updateEnvironment{}, fmt.Errorf("automatic updates require an existing local identity: %w", err)
	}
	environment := updateEnvironment{stateDir: stateDir, client: update.Client{ID: host.ID, Key: key}, local: update.LocalHost(stateDir, host.ID)}
	if declared, err := localNameRecord(ctx, stateDir); err == nil {
		environment.local.MachineName = declared.MachineName
	}
	environment.coordinator = environment.local
	if coordinator == "" || coordinator == localHostID() {
		return environment, nil
	}
	hosts, err := LoadHosts()
	if err != nil {
		return environment, err
	}
	remote, err := resolveHostTarget(hosts, coordinator)
	if err != nil {
		return environment, err
	}
	environment.coordinator = adoptedUpdateHost(remote)
	if err := environment.coordinator.Validate(); err != nil {
		return environment, fmt.Errorf("automatic coordinator: %w", err)
	}
	return environment, nil
}
func writeAutomaticJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write automatic status: %w", err)
	}
	return nil
}
