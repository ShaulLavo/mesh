package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/privacy"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updategate"
	"github.com/shaul/mesh/internal/updateinstall"
)

func (a *application) updateOperationCommand(action string, options *updateOptions) *cobra.Command {
	args, usage := cobra.ExactArgs(1), action+" [RUN]"
	if action == "status" {
		args, usage = cobra.MaximumNArgs(1), "status [RUN]"
	}
	var expected uint64
	var commandID string
	command := &cobra.Command{Use: usage, Short: updateActionDescription(action), Args: func(cmd *cobra.Command, arguments []string) error {
		if options.local {
			return cobra.NoArgs(cmd, arguments)
		}
		return args(cmd, arguments)
	},
		RunE: func(cmd *cobra.Command, args []string) error {
			if options.local {
				return a.localTargetOperation(cmd, action, options)
			}
			return a.runUpdateOperationCommand(cmd, args, action, options, expected, commandID)
		},
	}
	command.Flags().BoolVar(&options.local, "local", false, "read or control this machine's approved update target")
	if action != "status" {
		command.Flags().Uint64Var(&expected, "expected-policy-revision", 0, "expected automatic policy revision; required for automatic runs")
		command.Flags().StringVar(&commandID, "command-id", "", "exact administrative command identity for response-loss replay")
	}
	return command
}

func (a *application) runUpdateOperationCommand(cmd *cobra.Command, args []string, action string, options *updateOptions, expected uint64, commandID string) error {

	id := ""
	if len(args) > 0 {
		id = args[0]
	}
	if id != "" && !update.ValidRunID(id) {
		return errors.New("invalid update run ID")
	}
	resolved := *options
	var err error
	if action != "status" {
		resolved, _, err = a.resolveUpdateNameIntents(cmd.Context(), *options)
		if err != nil {
			return err
		}
	}
	open := openUpdateEnvironment
	if action == "status" {
		open = existingAutomaticEnvironment
	}
	environment, err := open(cmd.Context(), resolved.coordinator)
	if err != nil {
		return err
	}
	if a.dependencies.UpdateCaller != nil {
		environment.client = a.dependencies.UpdateCaller
	}
	output := updateOutput{cmd.OutOrStdout(), cmd.ErrOrStderr(), a.privacy, options.details}
	request, err := automaticOperationRequest(cmd, action, id, expected, commandID)
	if err != nil {
		return err
	}

	return a.runUpdateOperation(cmd.Context(), environment, action, request, options.json, output)
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

func (a *application) runUpdateOperation(ctx context.Context, environment updateEnvironment, action string, operation update.Operation, structured bool, output updateOutput) error {
	id := operation.ID
	if action == "status" && !structured && update.IsLocal(environment.coordinator) {
		if err := reportFinishedInstallationGate(environment.stateDir, id, output.diagnostic, output.privacy); err != nil {
			return err
		}
	}
	if action == "status" && id == "" {
		return listUpdateOperations(ctx, environment, structured, output)
	}
	if action == "cancel" && update.IsLocal(environment.coordinator) {
		if err := settleLocalTerminalInstallation(ctx, environment.stateDir, id); err != nil {
			return err
		}
	}
	var run update.Run
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := environment.client.Call(requestCtx, environment.coordinator, action, operation, &run)
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
	if err := printDeclaredUpdateRun(ctx, output.out, run, structured, output.details, output.privacy); err != nil {
		return err
	}
	return updateExit(run.ExitCode())
}

func reportFinishedInstallationGate(stateDir, id string, output io.Writer, masks ...*privacy.Mask) error {
	mask := presentationMask(masks)
	var gate *updategate.UpdatingError
	if !errors.As(updategate.Check(stateDir), &gate) || (id != "" && gate.Operation != id) {
		return nil
	}
	status, err := updateinstall.Read(stateDir)
	if err != nil {
		return fmt.Errorf("read held installation gate: %w", err)
	}
	if status.Request.ID != gate.Operation || !installationFinished(status.Phase) {
		return nil
	}
	_, err = fmt.Fprintf(output, "Update activation gate is held by finished run %s (%s). Run mesh update cancel %s to release it.\n", SafeTerminalText(mask.Value("update", gate.Operation)), SafeTerminalText(string(status.Phase)), SafeTerminalText(mask.Value("update", gate.Operation)))
	if err != nil {
		return fmt.Errorf("report finished installation gate: %w", err)
	}
	return nil
}

func settleLocalTerminalInstallation(ctx context.Context, stateDir, id string) error {
	status, err := updateinstall.Read(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read local installation settlement: %w", err)
	}
	if status.Request.ID != id || !installationFinished(status.Phase) {
		return nil
	}
	if status.Settings.StateDir != stateDir {
		return errors.New("installation journal belongs to another state directory")
	}
	config := status.Settings.Config()
	config.Probe = updatebootstrap.Probe(stateDir)
	engine, err := updateinstall.New(config)
	if err != nil {
		return fmt.Errorf("open local installation settlement: %w", err)
	}
	// Cancel rechecks the journal under the installer lock before releasing its gate.
	_, err = engine.Cancel(ctx, id, status.Request.Generation)
	if errors.Is(err, updateinstall.ErrAlreadyGranted) || errors.Is(err, updateinstall.ErrStaleGeneration) || errors.Is(err, updateinstall.ErrConflict) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("settle local terminal installation: %w", err)
	}
	return nil
}

func listUpdateOperations(ctx context.Context, environment updateEnvironment, structured bool, output updateOutput) error {
	var runs []update.Run
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := environment.client.Call(requestCtx, environment.coordinator, "runs", nil, &runs)
	cancel()
	if err != nil && update.IsLocal(environment.coordinator) {
		if status, readErr := updateinstall.Read(environment.stateDir); readErr == nil && status.Settings.ClientOnly {
			return printDeclaredUpdateRun(ctx, output.out, runFromInstallation(status), structured, output.details, output.privacy)
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
	return printListedUpdateOperations(ctx, environment, runs, structured, output)
}
func printListedUpdateOperations(ctx context.Context, environment updateEnvironment, runs []update.Run, structured bool, output updateOutput) error {
	updates := readUpdatePresentation(ctx, environment)
	if structured {
		if len(updates.Sources) == 0 && updates.Problem == "" {
			return writeAutomaticJSON(output.out, runs)
		}
		return writeAutomaticJSON(output.out, struct {
			Runs    []update.Run     `json:"runs"`
			Updates DashboardUpdates `json:"updates"`
		}{runs, updates})
	}
	if err := printDashboardUpdateRows(output.out, updates, output.privacy); err != nil {
		return err
	}
	if output.details && (len(updates.Sources) > 0 || updates.Problem != "") {
		if err := printUpdateDetails(output.out, updates, output.privacy); err != nil {
			return err
		}
	}
	if len(runs) == 0 {
		return writeUpdateLines(output.out, []string{"No saved update operations."})
	}
	for _, run := range runs {
		if err := printDeclaredUpdateRun(ctx, output.out, run, false, output.details, output.privacy); err != nil {
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
	if err := printDeclaredUpdateRun(ctx, output.out, run, structured, output.details, output.privacy); err != nil {
		return err
	}
	return updateExit(run.ExitCode())
}

func operateClientOnlyUpdate(ctx context.Context, environment updateEnvironment, status updateinstall.Status, action string, structured bool, output updateOutput) error {
	if action == "status" {
		run := runFromInstallation(status)
		if err := printDeclaredUpdateRun(ctx, output.out, run, structured, output.details, output.privacy); err != nil {
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
		if err := printDeclaredUpdateRun(ctx, output.out, runFromInstallation(status), structured, output.details, output.privacy); err != nil {
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

func automaticOperationRequest(cmd *cobra.Command, action, id string, expected uint64, commandID string) (update.Operation, error) {
	request := update.Operation{ID: id, CommandID: commandID}
	if cmd.Flags().Changed("expected-policy-revision") {
		request.ExpectedPolicyRevision = &expected
	}
	if commandID != "" || action == "status" {
		return request, nil
	}
	generated, err := update.NewAutomaticCommandID()
	if err != nil {
		return request, fmt.Errorf("allocate automatic operation command: %w", err)
	}
	request.CommandID = generated
	return request, nil
}

func readUpdatePresentation(ctx context.Context, environment updateEnvironment) DashboardUpdates {
	selection, err := readDashboardUpdateSelection(ctx, environment.stateDir, environment.local.ID)
	if err != nil {
		return DashboardUpdates{Problem: "Saved update status unavailable; inspect mesh update automatic status or mesh update config status with --details."}
	}
	watcher := newDashboardUpdateWatcher(environment.stateDir, environment.local.ID, environment.client)
	sources := watcher.observe(ctx, selection)
	current, err := readDashboardUpdateSelection(ctx, environment.stateDir, environment.local.ID)
	if err != nil || current.Fingerprint != selection.Fingerprint {
		return DashboardUpdates{Problem: "Saved update enrollment changed; read status again."}
	}
	return DashboardUpdates{Generation: 1, Sources: sources}
}
