package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
)

// PrivateNamesRequest is one explicit staging or live reconciliation pass.
type PrivateNamesRequest struct {
	ConfigPath   string
	StateDir     string
	DirectoryURL string
	AcceptTerms  bool
	Force        bool
}

// PrivateNamesFunc runs one operational private-name reconciliation pass.
type PrivateNamesFunc func(context.Context, PrivateNamesRequest) error

func reconcilePrivateNames(ctx context.Context, request PrivateNamesRequest) error {
	if ctx == nil {
		return errors.New("cli: reconcile private names with nil context")
	}
	_, privateKey, err := identity.LoadOrCreate(request.StateDir)
	if err != nil {
		return fmt.Errorf("cli: load renewer identity: %w", err)
	}
	runtime, err := dnsname.NewPrivateNamesRuntime(request.ConfigPath, dnsname.PrivateNamesRuntimeOptions{
		StateDir: request.StateDir, Signer: privateKey, DirectoryURL: request.DirectoryURL,
		AcceptTerms: request.AcceptTerms, Distribute: true,
	})
	if err != nil {
		return err
	}
	return runtime.Manager.RunOnce(ctx, request.Force)
}

func (a *application) privateNamesCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "private-names",
		Short: "Manage private DNS and wildcard certificates",
		Args:  cobra.NoArgs,
	}
	command.AddCommand(a.privateNamesReconcileCommand())
	return command
}

func (a *application) privateNamesReconcileCommand() *cobra.Command {
	var (
		configPath string
		staging    bool
		live       bool
		acceptTOS  bool
		force      bool
	)
	command := &cobra.Command{
		Use:   "reconcile",
		Short: "Reconcile DNS, renew, and distribute one certificate",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(configPath) == "" {
				return errors.New("private-names reconcile requires --config")
			}
			if staging == live {
				return errors.New("private-names reconcile requires exactly one of --staging or --live")
			}
			if !acceptTOS {
				return errors.New("private-names reconcile requires --accept-tos")
			}
			stateDir, err := paths.StateDir()
			if err != nil {
				return err
			}
			directoryURL := dnsname.LetsEncryptProductionURL
			environment := dnsname.EnvironmentLive
			if staging {
				directoryURL = dnsname.LetsEncryptStagingURL
				environment = dnsname.EnvironmentStaging
			}
			if err := a.dependencies.ReconcilePrivateNames(cmd.Context(), PrivateNamesRequest{
				ConfigPath: configPath, StateDir: stateDir, DirectoryURL: directoryURL, AcceptTerms: true, Force: force,
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "reconciled private names (%s)\n", environment)
			return nil
		},
	}
	command.Flags().StringVar(&configPath, "config", "", "private-name JSON config file")
	command.Flags().BoolVar(&staging, "staging", false, "use the isolated Let's Encrypt staging environment")
	command.Flags().BoolVar(&live, "live", false, "use the isolated Let's Encrypt production environment")
	command.Flags().BoolVar(&acceptTOS, "accept-tos", false, "accept the Let's Encrypt subscriber agreement")
	command.Flags().BoolVar(&force, "force", false, "issue a new certificate even when the current one is fresh")
	return command
}
