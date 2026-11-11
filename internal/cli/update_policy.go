package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/update"
	"github.com/spf13/cobra"
)

type hostPolicyOptions struct {
	expected uint64
	after    []string
	soak     time.Duration
}

const (
	hostPolicyShow = "show"
	hostPolicySet  = "set"
	hostPolicyOff  = "off"
)

func (a *application) updatePolicyCommand(options *updateOptions) *cobra.Command {
	command := &cobra.Command{Use: "policy", Short: "Manage automatic updates on this machine", Args: cobra.NoArgs}
	for _, action := range []string{hostPolicyShow, hostPolicySet, hostPolicyOff} {
		command.AddCommand(a.hostPolicyCommand(options, action))
	}
	return command
}

func (a *application) hostPolicyCommand(options *updateOptions, action string) *cobra.Command {
	selected := hostPolicyOptions{}
	short := map[string]string{hostPolicyShow: "Show this machine's update policy", hostPolicySet: "Enable automatic updates on this machine", hostPolicyOff: "Stop granting automatic updates on this machine"}
	command := &cobra.Command{Use: action, Short: short[action], Args: cobra.NoArgs}
	command.RunE = func(cmd *cobra.Command, _ []string) error {
		return a.runHostPolicy(cmd, options, action, selected)
	}
	if action != hostPolicyShow {
		command.Flags().Uint64Var(&selected.expected, "expected-revision", 0, "policy revision reported by policy show")
	}
	if action == hostPolicySet {
		command.Flags().StringArrayVar(&selected.after, "after", nil, "wait for a named machine before updating; may repeat")
		command.Flags().DurationVar(&selected.soak, "soak", 0, "time to wait after each predecessor updates")
	}
	return command
}

func (a *application) runHostPolicy(cmd *cobra.Command, options *updateOptions, action string, selected hostPolicyOptions) error {
	if options.coordinator != "" {
		return errors.New("update policy is local; omit --coordinator and run this command on the machine to change")
	}
	if action != hostPolicyShow && !cmd.Flags().Changed("expected-revision") {
		return errors.New("run mesh update policy show, then pass its revision with --expected-revision")
	}
	environment, err := existingAutomaticEnvironment(cmd.Context(), "")
	if err != nil {
		return err
	}
	if a.dependencies.UpdateCaller != nil {
		environment.client = a.dependencies.UpdateCaller
	}
	policy, err := a.callHostPolicy(cmd.Context(), environment, action, selected)
	if err != nil {
		return err
	}
	if options.json {
		return writeAutomaticJSON(cmd.OutOrStdout(), policy)
	}
	return printHostPolicy(cmd.OutOrStdout(), policy, action, options.details)
}

func (a *application) callHostPolicy(ctx context.Context, environment updateEnvironment, action string, selected hostPolicyOptions) (update.HostPolicy, error) {
	var policy update.HostPolicy
	if err := environment.client.Call(ctx, environment.local, "host-policy.show", nil, &policy); err != nil {
		return policy, fmt.Errorf("read local update policy: %w", err)
	}
	if action == hostPolicyShow {
		return policy, nil
	}
	policy.Mode = update.HostManual
	if action == hostPolicySet {
		after, err := a.hostPolicyPredecessors(ctx, environment, selected.after)
		if err != nil {
			return policy, err
		}
		policy.Mode, policy.After, policy.Soak = update.HostAutomatic, after, selected.soak
	}
	change := update.HostPolicyChange{ExpectedRevision: selected.expected, Policy: policy}
	if err := environment.client.Call(ctx, environment.local, "host-policy.set", change, &policy); err != nil {
		return policy, fmt.Errorf("save local update policy: %w", err)
	}
	return policy, nil
}

func (a *application) hostPolicyPredecessors(ctx context.Context, environment updateEnvironment, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	hosts, err := LoadHosts()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(names))
	for _, name := range names {
		target, err := resolveArgumentWithRefresh(ctx, name, hosts, a.dependencies.DialControl)
		if err != nil {
			return nil, err
		}
		if target.Host == nil {
			return nil, errors.New("--after requires a machine name or exact host identity")
		}
		if target.Host.ID == environment.local.ID {
			return nil, errors.New("a machine cannot wait for itself before updating")
		}
		if err := a.verifyHostPolicyName(ctx, *target.Host); err != nil {
			return nil, err
		}
		ids = append(ids, target.Host.MeshIdentity)
	}
	return ids, nil
}

func (a *application) verifyHostPolicyName(ctx context.Context, host HostRecord) error {
	if host.targetName == "" {
		return nil
	}
	return verifyUpdateNameIntents(ctx, []HostRecord{host}, a.dependencies.DialControl)
}

func printHostPolicy(out io.Writer, policy update.HostPolicy, action string, details bool) error {
	_, err := fmt.Fprintf(out, "Automatic updates: %s\nPolicy revision: %d\n", hostPolicyMode(policy.Mode), policy.Revision)
	if err != nil {
		return fmt.Errorf("print host policy: %w", err)
	}
	if len(policy.After) > 0 {
		_, err = fmt.Fprintf(out, "Machines to wait for: %d\nWait after each update: %s\n", len(policy.After), policy.Soak)
	}
	if details && err == nil {
		_, err = fmt.Fprintln(out, "Wait for identities: "+strings.Join(policy.After, ", "))
	}
	if action == hostPolicyOff && err == nil {
		_, err = fmt.Fprintln(out, "No further automatic work will be granted. An update already granted may finish, including recovery.")
	}
	if err != nil {
		return fmt.Errorf("print host policy: %w", err)
	}
	return nil
}

func hostPolicyMode(mode update.HostMode) string {
	if mode == update.HostAutomatic {
		return "on"
	}
	if mode == update.HostManual {
		return hostPolicyOff
	}
	return strings.TrimSpace(string(mode))
}
