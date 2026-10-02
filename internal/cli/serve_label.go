package cli

import (
	"context"
	"fmt"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/spf13/cobra"
)

func serviceDisplayName(service protocol.ServiceInfo) string {
	if service.DisplayName != "" {
		return service.DisplayName
	}
	return service.Name
}

func (a *application) serveLabelCommand() *cobra.Command {
	var hostAlias string
	command := &cobra.Command{
		Use:   "label ROUTE NAME",
		Short: "Set the display name of an existing service",
		Args:  exactArgs(2, "a route and display name", "mesh serve label :5173 'Fregat dev' --host pc"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runServeLabel(cmd, args[0], args[1], hostAlias)
		},
	}
	command.Flags().StringVar(&hostAlias, "host", "", "host alias when more than one host owns ROUTE")
	return command
}

func (a *application) runServeLabel(cmd *cobra.Command, route, label, hostAlias string) error {
	if label == "" {
		return fmt.Errorf("display name must contain text")
	}
	if err := meshserve.ValidateDisplayName(label); err != nil {
		return fmt.Errorf("invalid service label: %w", err)
	}
	name, err := serviceNameFromRoute(route)
	if err != nil {
		return err
	}
	cache, err := OpenCatalogCache(cmd.Context())
	if err != nil {
		return err
	}
	defer cache.Close() //nolint:errcheck // command result takes precedence
	selected, _, err := a.resolveServiceOwner(cmd, cache, route, name, hostAlias, defaultServiceListTimeout)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), serviceMutationTimeout)
	defer cancel()
	response, _, err := remoteServiceRequest(ctx, selected.Host, a.dependencies.DialControl, protocol.Control{
		Type: protocol.TypeServiceLabel, ServiceName: name, ServiceDisplayName: label,
	}, nil)
	if err != nil {
		return err
	}
	if response.Type == protocol.TypeError {
		return remoteServiceResponseError(selected.Host, "service label", response)
	}
	if response.Type != protocol.TypeServiceLabeled || response.Service == nil || response.Service.Name != name || response.Service.DisplayName != label {
		return fmt.Errorf("host %s did not acknowledge the service label; update Mesh there first", selected.Host.Alias)
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s on %s is now %s\n", a.privacy.Value("route", route), a.privacy.Value("host", selected.Host.Alias), a.privacy.Value("name", label)); err != nil {
		return fmt.Errorf("write service label: %w", err)
	}
	return nil
}
