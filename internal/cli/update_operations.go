package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func (a *application) updateOperationCommand(action string, options *updateOptions) *cobra.Command {
	args, usage := cobra.ExactArgs(1), action+" RUN"
	if action == "status" {
		args, usage = cobra.MaximumNArgs(1), "status [RUN]"
	}
	return &cobra.Command{Use: usage, Short: updateActionDescription(action), Args: args,
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			if id != "" && !update.ValidRunID(id) {
				return errors.New("invalid update run ID")
			}
			environment, err := openUpdateEnvironment(options.coordinator)
			if err != nil {
				return err
			}
			if a.dependencies.UpdateCaller != nil {
				environment.client = a.dependencies.UpdateCaller
			}
			output := updateOutput{cmd.OutOrStdout(), cmd.ErrOrStderr()}
			return a.runUpdateOperation(cmd.Context(), environment, action, id, options.json, output)
		},
	}
}

func updateActionDescription(action string) string {
	switch action {
	case "retry":
		return "Retry unfinished targets at the originally approved release"
	case "cancel":
		return "Stop pending updates; issued activation grants may still finish"
	default:
		return "Show persisted fleet update progress"
	}
}

func (a *application) runUpdateOperation(ctx context.Context, environment updateEnvironment, action, id string, structured bool, output updateOutput) error {
	if action == "status" && id == "" {
		return listUpdateOperations(ctx, environment, structured, output)
	}
	var run update.Run
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := environment.client.Call(requestCtx, environment.coordinator, action, update.Operation{ID: id}, &run)
	cancel()
	if err != nil && update.IsLocal(environment.coordinator) {
		return a.localUpdateOperation(ctx, environment, action, id, structured, output, err)
	}
	if err != nil {
		return err
	}
	if action == "retry" {
		return observeUpdate(ctx, environment, run, structured, output)
	}
	if err := printUpdateRun(output.out, run, structured); err != nil {
		return err
	}
	return updateExit(run.ExitCode())
}

func listUpdateOperations(ctx context.Context, environment updateEnvironment, structured bool, output updateOutput) error {
	var runs []update.Run
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := environment.client.Call(requestCtx, environment.coordinator, "runs", nil, &runs)
	cancel()
	if err != nil && update.IsLocal(environment.coordinator) {
		if status, readErr := updateinstall.Read(environment.stateDir); readErr == nil && status.Settings.ClientOnly {
			return printUpdateRun(output.out, runFromInstallation(status), structured)
		}
		store, openErr := update.OpenStore(environment.stateDir)
		if openErr != nil {
			return openErr
		}
		runs, err = store.List()
	}
	if err != nil {
		return err
	}
	if structured {
		return json.NewEncoder(output.out).Encode(runs)
	}
	if len(runs) == 0 {
		_, err := fmt.Fprintln(output.out, "No saved update operations.")
		return err
	}
	for _, run := range runs {
		if err := printUpdateRun(output.out, run, false); err != nil {
			return err
		}
	}
	return nil
}

func (a *application) localUpdateOperation(ctx context.Context, environment updateEnvironment, action, id string, structured bool, output updateOutput, remoteErr error) error {
	status, err := updateinstall.Read(environment.stateDir)
	if err == nil && status.Settings.ClientOnly && status.Request.ID == id {
		return operateClientOnlyUpdate(ctx, environment, status, action, structured, output)
	}
	if run, readErr := readFirstCoordinatorRun(environment, id); readErr == nil {
		return a.operateFirstCoordinator(ctx, environment, run, action, structured, output)
	}
	if action != "status" {
		return remoteErr
	}
	store, err := update.OpenStore(environment.stateDir)
	if err != nil {
		return err
	}
	run, err := store.Read(id)
	if err != nil {
		return remoteErr
	}
	if !structured {
		_, _ = fmt.Fprintln(output.diagnostic, "Coordinator unavailable. Showing the last persisted local state.")
	}
	if err := printUpdateRun(output.out, run, structured); err != nil {
		return err
	}
	return updateExit(run.ExitCode())
}

func operateClientOnlyUpdate(ctx context.Context, environment updateEnvironment, status updateinstall.Status, action string, structured bool, output updateOutput) error {
	if action == "status" {
		run := runFromInstallation(status)
		if err := printUpdateRun(output.out, run, structured); err != nil {
			return err
		}
		return updateExit(run.ExitCode())
	}
	config := status.Settings.Config()
	engine, err := updateinstall.New(config)
	if err != nil {
		return err
	}
	if action == "cancel" {
		if err := removeClientOnlyApproval(status); err != nil {
			return err
		}
		status, err = engine.Cancel(ctx, status.Request.ID, status.Request.Generation)
		if err != nil && !errors.Is(err, updateinstall.ErrAlreadyGranted) {
			return err
		}
		if err != nil {
			status.Error = "Activation was already authorized and may still finish."
		}
		if err := printUpdateRun(output.out, runFromInstallation(status), structured); err != nil {
			return err
		}
		return updateExit(runFromInstallation(status).ExitCode())
	}
	if action != "retry" {
		return errors.New("unsupported local update operation")
	}
	if _, err := update.EnsureHelper(ctx, environment.stateDir); err != nil {
		return err
	}
	if status.Phase == updateinstall.RolledBack {
		status, err = engine.Retry(ctx, status.Request.ID, status.Request.Generation)
		if err != nil {
			return err
		}
	}
	if installationFinished(status.Phase) {
		return errors.New("this installation needs intervention or a new approved update")
	}
	if err := saveClientOnlyApproval(environment.stateDir, status.Request); err != nil {
		return err
	}
	return observeClientOnlyUpdate(ctx, environment.stateDir, status, structured, output)
}

func removeClientOnlyApproval(status updateinstall.Status) error {
	err := os.Remove(clientOnlyApprovalPath(status.Settings.StateDir, status.Request.ID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
