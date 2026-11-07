package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/update"
	"github.com/spf13/cobra"
)

const (
	dashboardFeedEnabled  = "enabled"
	dashboardFeedDisabled = "disabled"
	dashboardFeedInvalid  = "invalid"
	configurationApply    = "apply"
	configurationPreview  = "preview"
	configurationStatus   = "status"
	configurationRetry    = "retry"
	configurationCancel   = "cancel"
)

type configurationOptions struct {
	file, commandID            string
	expected                   uint64
	yes, usage, retire, domain bool
}

func (a *application) updateConfigurationCommand() *cobra.Command {
	command := &cobra.Command{Use: "config", Short: "Deliver explicitly approved feed and domain settings", Args: cobra.NoArgs}
	command.AddCommand(a.configurationGrantCommand(true), a.configurationGrantCommand(false))
	for _, action := range []string{configurationPreview, configurationApply, configurationStatus, configurationRetry, configurationCancel} {
		command.AddCommand(a.configurationActionCommand(action))
	}
	return command
}
func (a *application) configurationGrantCommand(allow bool) *cobra.Command {
	options := configurationOptions{}
	command := &cobra.Command{Use: "revoke", Short: "Withdraw this account's configuration grant", Args: cobra.NoArgs}
	if allow {
		command.Use, command.Short, command.Args = "allow COORDINATOR_ID", "Allow selected configuration groups from an exact coordinator", cobra.ExactArgs(1)
	}
	command.Flags().Uint64Var(&options.expected, "expected-revision", 0, "expected target-local configuration grant revision")
	if allow {
		command.Flags().BoolVar(&options.usage, "usage-feed", false, "allow dashboard usage-feed URL delivery")
		command.Flags().BoolVar(&options.domain, "domain-policy", false, "allow primary domain and complete aliases; preserve existing accepted domains")
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		return a.runConfigurationGrant(cmd, args, allow, options)
	}

	return command
}
func (a *application) runConfigurationGrant(cmd *cobra.Command, args []string, allow bool, options configurationOptions) error {
	if coordinator, _ := cmd.Flags().GetString("coordinator"); coordinator != "" {
		return errors.New("configuration grants are target-local; omit --coordinator")
	}
	if !cmd.Flags().Changed("expected-revision") || allow && !options.usage && !options.domain {
		return errors.New("--expected-revision and at least one explicit configuration group are required for allowance")
	}
	environment, err := existingAutomaticEnvironment(cmd.Context(), "")
	if err != nil {
		return err
	}
	store, err := update.OpenStore(environment.stateDir)
	if err != nil {
		return fmt.Errorf("open configuration grant: %w", err)
	}
	coordinator := ""
	if allow {
		coordinator = args[0]
	}
	grant, err := store.SetConfigurationGrant(cmd.Context(), options.expected, environment.local.ID, coordinator, allow && options.usage, allow && options.domain)
	if err != nil {
		return fmt.Errorf("change configuration grant: %w", err)
	}
	return writeAutomaticJSON(cmd.OutOrStdout(), grant)
}
func (a *application) configurationActionCommand(action string) *cobra.Command {
	options := configurationOptions{}
	command := &cobra.Command{Use: action, Short: configurationDescription(action), Args: cobra.NoArgs}
	if action == configurationPreview || action == configurationApply {
		command.Flags().StringVar(&options.file, "file", "", "exact selection or previously returned proposal JSON file")
	} else {
		command.Use += " CHANGE_ID"
		command.Args = cobra.ExactArgs(1)
	}
	if action == configurationApply {
		command.Flags().BoolVar(&options.yes, "yes", false, "approve the exact verified proposal")
	}
	if action == configurationApply || action == configurationRetry || action == configurationCancel {
		command.Flags().StringVar(&options.commandID, "command-id", "", "administrative identity to reuse after response loss")
	}
	if action == configurationRetry || action == configurationCancel {
		command.Flags().Uint64Var(&options.expected, "expected-revision", 0, "expected configuration administrative revision")
	}
	if action == configurationCancel {
		command.Flags().BoolVar(&options.retire, "retire", false, "local coordinator only: cancel, settle and archive without taking ownership")
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		return a.runConfigurationAction(cmd, action, args, options)
	}
	return command
}
func (a *application) runConfigurationAction(cmd *cobra.Command, action string, args []string, options configurationOptions) error {
	coordinator, _ := cmd.Flags().GetString("coordinator")
	environment, err := existingAutomaticEnvironment(cmd.Context(), coordinator)
	if err != nil {
		return err
	}
	if a.dependencies.UpdateCaller != nil {
		environment.client = a.dependencies.UpdateCaller
	}
	if action == configurationPreview {
		var selection update.ConfigurationSelection
		if err = readConfigurationInput(options.file, &selection); err != nil {
			return err
		}
		var proposal update.ConfigurationProposal
		if err = environment.client.Call(cmd.Context(), environment.coordinator, "configuration.preview", selection, &proposal); err != nil {
			return fmt.Errorf("preview configuration: %w", err)
		}
		return writeAutomaticJSON(cmd.OutOrStdout(), proposal)
	}
	if action == configurationApply {
		return a.approveConfiguration(cmd, environment, options)
	}
	input := any(struct {
		ID string `json:"id"`
	}{args[0]})
	if action != configurationStatus {
		if !cmd.Flags().Changed("expected-revision") {
			return errors.New("--expected-revision is required")
		}
		id, err := configurationCommandID(options.commandID)
		if err != nil {
			return err
		}
		input = update.ConfigurationCommand{ID: id, ChangeID: args[0], ExpectedRevision: options.expected, Retire: options.retire}
	}
	var status update.ConfigurationStatus
	if err = environment.client.Call(cmd.Context(), environment.coordinator, "configuration."+action, input, &status); err != nil {
		return fmt.Errorf("configuration %s: %w", action, err)
	}
	return writeAutomaticJSON(cmd.OutOrStdout(), status)
}
func (a *application) approveConfiguration(cmd *cobra.Command, environment updateEnvironment, options configurationOptions) error {
	var proposal update.ConfigurationProposal
	if err := readConfigurationInput(options.file, &proposal); err != nil {
		return err
	}
	if !options.yes {
		return errors.New("review the exact preview and pass --yes to approve configuration delivery")
	}
	id, err := configurationCommandID(options.commandID)
	if err != nil {
		return err
	}
	approval := update.ConfigurationApproval{CommandID: id, Proposal: proposal}
	var status update.ConfigurationStatus
	if err = environment.client.Call(cmd.Context(), environment.coordinator, "configuration.approve", approval, &status); err != nil {
		return fmt.Errorf("approve configuration: %w", err)
	}
	return writeAutomaticJSON(cmd.OutOrStdout(), status)
}
func readConfigurationInput(path string, value any) error {
	if path == "" {
		return errors.New("--file is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve configuration input: %w", err)
	}
	root, err := os.OpenRoot(filepath.Dir(absolute))
	if err != nil {
		return fmt.Errorf("open configuration input directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(absolute))
	if err != nil {
		return fmt.Errorf("open configuration input: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, update.MaximumConfigurationProposal+1))
	if err != nil {
		return fmt.Errorf("read configuration input: %w", err)
	}
	if err := update.DecodeConfiguration(data, value); err != nil {
		return fmt.Errorf("decode configuration input: %w", err)
	}
	return nil
}
func configurationCommandID(id string) (string, error) {
	if id != "" {
		if !update.ValidRunID(id) {
			return "", errors.New("invalid --command-id")
		}
		return id, nil
	}
	result, err := update.NewAutomaticCommandID()
	if err != nil {
		return "", fmt.Errorf("allocate configuration command: %w", err)
	}
	return result, nil
}
func configurationDescription(action string) string {
	switch action {
	case configurationPreview:
		return "Capture exact targets, selected values and recipient grants without delivery"
	case configurationApply:
		return "Approve the exact verified proposal for durable delivery"
	case configurationRetry:
		return "Explicitly retry the same approver's unchanged configuration"
	case configurationCancel:
		return "Stop new dispatch intentions; earlier issued work may still finish"
	default:
		return "Show saved, issued and actual dashboard observations separately"
	}
}
