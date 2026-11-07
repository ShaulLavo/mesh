package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/privacy"
	"github.com/shaul/mesh/internal/update"
	"github.com/spf13/cobra"
)

const (
	automaticEnable       = "enable"
	automaticStatusAction = "status"
	automaticRevoke       = "revoke"
	automaticAcknowledge  = "acknowledge-stop"
)

type automaticConsentOptions struct {
	file            string
	epoch, expected uint64
	allow           bool
}
type automaticPolicyOptions struct {
	action, commandID string
	expected          uint64
	yes, take         bool
}

func (a *application) updateAutomaticCommand(options *updateOptions) *cobra.Command {
	command := &cobra.Command{Use: "automatic", Short: "Manage exact saved-fleet unattended update consent", Args: cobra.NoArgs}
	command.AddCommand(a.automaticConsentCommand(options, true), a.automaticConsentCommand(options, false))
	for _, action := range []string{automaticStatusAction, automaticEnable, "disable", automaticAcknowledge} {
		command.AddCommand(a.automaticPolicyCommand(options, action))
	}
	return command
}
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
func (a *application) automaticConsentCommand(options *updateOptions, allow bool) *cobra.Command {
	selected := automaticConsentOptions{allow: allow}
	command := &cobra.Command{Use: automaticRevoke, Short: "Withdraw this account's unattended binary consent", Args: cobra.NoArgs}
	if allow {
		command.Use = "authorize COORDINATOR_ID"
		command.Short = "Bind this account to an exact coordinator, fleet and epoch"
		command.Args = cobra.ExactArgs(1)
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		return a.runAutomaticConsent(cmd, args, options, selected)
	}
	command.Flags().Uint64Var(&selected.expected, "expected-revision", 0, "expected local recipient consent revision")
	if allow {
		command.Flags().StringVar(&selected.file, "fleet", "", "exact saved fleet file")
		command.Flags().Uint64Var(&selected.epoch, "epoch", 0, "exact pending coordinator enrollment epoch")
	}
	return command
}
func (a *application) runAutomaticConsent(cmd *cobra.Command, args []string, options *updateOptions, selected automaticConsentOptions) error {
	if options.coordinator != "" {
		return errors.New("automatic recipient consent is target-local; omit --coordinator")
	}
	if !cmd.Flags().Changed("expected-revision") {
		return errors.New("--expected-revision is required")
	}
	environment, err := existingAutomaticEnvironment(cmd.Context(), "")
	if err != nil {
		return err
	}
	change := update.ConsentChange{LocalHostID: environment.local.ID, Enabled: selected.allow, Epoch: selected.epoch}
	if selected.allow {
		change.CoordinatorID = args[0]
		change.Fleet, err = update.ReadFleet(selected.file)
		if err != nil {
			return fmt.Errorf("read automatic recipient fleet: %w", err)
		}
	}
	store, err := update.OpenStore(environment.stateDir)
	if err != nil {
		return fmt.Errorf("open automatic consent: %w", err)
	}
	consent, err := store.SetRecipientConsent(cmd.Context(), selected.expected, change)
	if err != nil {
		return fmt.Errorf("change automatic consent: %w", err)
	}
	return writeAutomaticJSON(cmd.OutOrStdout(), consent)
}
func (a *application) automaticPolicyCommand(options *updateOptions, action string) *cobra.Command {
	selected := automaticPolicyOptions{action: action}
	command := &cobra.Command{Use: action, Short: automaticActionDescription(action), Args: cobra.NoArgs}
	if action == automaticAcknowledge {
		command.Use += " RUN_OR_POLICY_ID"
		command.Args = cobra.ExactArgs(1)
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		return a.runAutomaticPolicy(cmd, args, options, selected)
	}
	if action != automaticStatusAction {
		command.Flags().Uint64Var(&selected.expected, "expected-revision", 0, "expected coordinator policy revision")
		command.Flags().StringVar(&selected.commandID, "command-id", "", "exact administrative command identity for response-loss replay")
	}
	if action == automaticEnable {
		command.Flags().BoolVar(&selected.yes, "yes", false, "approve unattended updates for the coordinator's exact saved fleet")
		command.Flags().BoolVar(&selected.take, "take-ownership", false, "local coordinator recovery after disabled, settled stop acknowledgement")
	}
	return command
}
func (a *application) runAutomaticPolicy(cmd *cobra.Command, args []string, options *updateOptions, selected automaticPolicyOptions) error {
	environment, err := existingAutomaticEnvironment(cmd.Context(), options.coordinator)
	if err != nil {
		return err
	}
	if a.dependencies.UpdateCaller != nil {
		environment.client = a.dependencies.UpdateCaller
	}
	var status update.AutomaticStatus
	if _, err = dashboardStatusCall(cmd.Context(), environment.client, environment.coordinator, "automatic.status", nil, &status); err != nil {
		return automaticUnavailable(cmd, environment, err, options.json, a.privacy)
	}
	if selected.action == automaticStatusAction {
		return printAutomaticStatus(cmd, status, options.json, a.privacy)
	}
	request, err := a.approveAutomaticCommand(cmd, args, status, selected)
	if err != nil {
		return err
	}
	if err = environment.client.Call(cmd.Context(), environment.coordinator, "automatic.change", request, &status); err != nil {
		return fmt.Errorf("change automatic policy: %w", err)
	}
	return printAutomaticStatus(cmd, status, options.json, a.privacy)
}
func (a *application) approveAutomaticCommand(cmd *cobra.Command, args []string, status update.AutomaticStatus, selected automaticPolicyOptions) (update.AutomaticCommand, error) {
	if !cmd.Flags().Changed("expected-revision") {
		return update.AutomaticCommand{}, errors.New("--expected-revision is required")
	}
	if selected.action == automaticEnable {
		if status.SavedFleet == nil {
			return update.AutomaticCommand{}, errors.New("coordinator saved fleet unavailable; inspect automatic status")
		}
		if err := writeAutomaticJSON(cmd.ErrOrStderr(), status); err != nil {
			return update.AutomaticCommand{}, err
		}
		if !selected.yes && !a.confirmAutomatic(cmd) {
			return update.AutomaticCommand{}, errors.New("automatic enable cancelled; use --yes to approve explicitly")
		}
	}
	id := selected.commandID
	if id == "" {
		var err error
		id, err = update.NewAutomaticCommandID()
		if err != nil {
			return update.AutomaticCommand{}, fmt.Errorf("allocate automatic command: %w", err)
		}
	}
	request := update.AutomaticCommand{ID: id, Action: selected.action, ExpectedRevision: selected.expected, TakeOwnership: selected.take, ExpectedMembership: status.SavedMembership}
	if len(args) > 0 {
		request.RunID = args[0]
	}
	return request, nil
}
func automaticActionDescription(action string) string {
	switch action {
	case automaticEnable:
		return "Enable exact saved-fleet release tracking; wait for local recipient consent"
	case "disable":
		return "Stop new automatic dispatch; issued work may still finish"
	case automaticAcknowledge:
		return "Acknowledge a settled run or disabled pre-run policy stop; stay disabled"
	default:
		return "Show policy, enrollment, stop, continuation and pinned run status"
	}
}
func (a *application) confirmAutomatic(cmd *cobra.Command) bool {
	if !a.updateInteractive() {
		return false
	}
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Enable unattended releases for the coordinator's saved fleet? [y/N]")
	answer, _ := bufio.NewReader(a.dependencies.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(answer), "y")
}
func writeAutomaticJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write automatic status: %w", err)
	}
	return nil
}
func printAutomaticStatus(cmd *cobra.Command, status update.AutomaticStatus, structured bool, masks ...*privacy.Mask) error {
	details, _ := cmd.Flags().GetBool("details")
	return printAutomaticStatusOutput(cmd.OutOrStdout(), status, structured, details, presentationMask(masks))
}
func automaticUnavailable(cmd *cobra.Command, environment updateEnvironment, cause error, structured bool, masks ...*privacy.Mask) error {
	if !update.IsLocal(environment.coordinator) {
		return fmt.Errorf("automatic status transport: %w", cause)
	}
	if _, err := os.Stat(filepath.Join(environment.stateDir, "updates", "runs")); err != nil {
		return fmt.Errorf("automatic status unavailable: %w", cause)
	}
	store, err := update.OpenStore(environment.stateDir)
	if err != nil {
		return fmt.Errorf("open saved automatic status: %w", err)
	}
	status, err := store.ReadAutomatic(cmd.Context())
	if err != nil {
		status.State = "state unavailable"
		status.Problem = err.Error()
		status.Revision = nil
	}
	status.TransportProblem = cause.Error()
	if err := printAutomaticStatus(cmd, status, structured, masks...); err != nil {
		return err
	}
	return fmt.Errorf("coordinator unavailable; showing saved automatic state: %w", cause)
}
