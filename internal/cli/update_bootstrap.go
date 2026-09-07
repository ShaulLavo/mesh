package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

func updateBootstrapCommand() *cobra.Command {
	var encoded string
	command := &cobra.Command{Use: "update-bootstrap", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		request, err := updatebootstrap.Decode(encoded)
		if err != nil {
			return err
		}
		stateDir, err := paths.StateDir()
		if err != nil {
			return err
		}
		cache, err := update.CacheDir()
		if err != nil {
			return err
		}
		status, err := updatebootstrap.Run(cmd.Context(), request, updatebootstrap.Config{StateDir: stateDir, CacheDir: cache, RequiredMount: os.Getenv("MESH_UPDATE_REQUIRED_MOUNT"), Enroll: func(stateDir, id string) error { return update.Trust(stateDir, id, true) }})
		if err != nil {
			return err
		}
		status, err = waitBootstrapInstallation(cmd.Context(), stateDir, status)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
	}}
	command.Flags().StringVar(&encoded, "request-base64", "", "approved legacy bootstrap request")
	return command
}

func waitBootstrapInstallation(ctx context.Context, stateDir string, status updateinstall.Status) (updateinstall.Status, error) {
	if installationFinished(status.Phase) {
		return status, nil
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-ticker.C:
		}
		current, err := updateinstall.Read(stateDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return status, err
		}
		if current.Request.ID != status.Request.ID || current.Request.TargetID != status.Request.TargetID || current.Request.Generation != status.Request.Generation || current.Request.Manifest.Digest() != status.Request.Manifest.Digest() {
			return status, errors.New("bootstrap installation changed while awaiting its terminal receipt")
		}
		status = current
		if installationFinished(status.Phase) {
			return status, nil
		}
	}
}

func updateBootstrapStatusCommand() *cobra.Command {
	var stateDir, id, digest, target string
	var generation uint64
	command := &cobra.Command{Use: "update-bootstrap-status", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		status, err := updatebootstrap.ReadStatus(stateDir, id, generation, digest, target)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
	}}
	command.Flags().StringVar(&stateDir, "state-dir", "", "existing Mesh state directory")
	command.Flags().StringVar(&id, "id", "", "approved operation ID")
	command.Flags().Uint64Var(&generation, "generation", 0, "approved target generation")
	command.Flags().StringVar(&digest, "manifest-digest", "", "approved manifest digest")
	command.Flags().StringVar(&target, "target", "", "pinned target identity")
	return command
}
