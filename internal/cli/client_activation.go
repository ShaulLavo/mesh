package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/updateinstall"
	"github.com/spf13/cobra"
)

func activateClientConfig(command *cobra.Command) error {
	top := command
	for top.Parent() != nil && top.Parent().Parent() != nil {
		top = top.Parent()
	}
	switch top.Name() {
	case "dashboard", "session-worker", "agent", "agent-hook", "agent-resume", "update-helper", "update-notice-check", "update-bootstrap", "update-bootstrap-status", "completion", "help":
		return nil
	case "version":
		return retireValidatingClient(command.Context())
	default:
		return retireClientAliases(command.Context())
	}
}

func retireValidatingClient(ctx context.Context) error {
	stateDir, err := paths.StateDirPath()
	if err != nil {
		return fmt.Errorf("locate client activation state: %w", err)
	}
	status, err := updateinstall.ReadContext(ctx, stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check client activation: %w", err)
	}
	if status.Settings.StateDir != stateDir {
		return errors.New("client installation journal belongs to another state directory")
	}
	if !status.Settings.ClientOnly || status.Phase != updateinstall.Validating {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executing client: %w", err)
	}
	executing, err := os.Stat(executable) //nolint:gosec // executable selected by the OS
	if err != nil {
		return fmt.Errorf("inspect executing client: %w", err)
	}
	installed, err := os.Stat(status.Settings.Executable) //nolint:gosec // executable from the secure local installation journal
	if err != nil {
		return fmt.Errorf("inspect installed client: %w", err)
	}
	if !os.SameFile(executing, installed) {
		return nil
	}
	// The retained helper probes the installed client with version --json.
	// Staged candidate metadata reads must leave the client's configuration alone.
	return retireClientAliases(ctx)
}
