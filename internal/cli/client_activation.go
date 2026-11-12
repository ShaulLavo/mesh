package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
	"github.com/spf13/cobra"
)

func activateClientConfig(command *cobra.Command) error {
	if command.Name() == skillShowVerb && command.Parent() != nil && command.Parent().Name() == "skill" {
		return nil
	}
	switch command.Name() {
	case "approve-checked", "approve-fleet":
		return nil
	}
	top := command
	for top.Parent() != nil && top.Parent().Parent() != nil {
		top = top.Parent()
	}
	if command.Name() == adoptionCommandName || command.Parent() != nil && command.Parent().Name() == adoptionCommandName {
		return nil
	}
	switch top.Name() {
	case "hosts", agentInspectCommandName, "profile", "dashboard", "session-worker", "agent", "agent-hook", "agent-resume", "update-helper", "update-notice-check", "update-bootstrap", "update-bootstrap-status", "completion", "help", "version":
		return nil
	default:
		return retireClientAliases(command.Context())
	}
}

func clientConfigCanRetire(ctx context.Context) (bool, error) {
	stateDir, err := paths.StateDirPath()
	if err != nil {
		return false, fmt.Errorf("locate client activation state: %w", err)
	}
	pending, err := update.AdoptionPending(ctx, stateDir)
	if err != nil {
		return false, fmt.Errorf("check client adoption cutover: %w", err)
	}
	if pending {
		return false, nil
	}
	status, err := updateinstall.ReadContext(ctx, stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("check client configuration cutover: %w", err)
	}
	if status.Settings.StateDir != stateDir {
		return false, errors.New("client installation journal belongs to another state directory")
	}
	// The retained executable still reads aliases until the update commits.
	// Version probes and rollback must never delete that executable's input.
	return status.Phase == updateinstall.Committed, nil
}
