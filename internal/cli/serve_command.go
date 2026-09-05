package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/muesli/cancelreader"
	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

const (
	defaultServiceListTimeout = 2 * time.Second
	maximumServiceListTimeout = 30 * time.Second
	serviceMutationTimeout    = 30 * time.Second
)

// PublicConfirmation contains the exact origin-authoritative facts shown
// before a public mutation.
type PublicConfirmation struct {
	Host                HostRecord
	Service             protocol.ServiceInfo
	FileCount           uint64
	URL                 string
	CredentialsOverride bool
}

// ConfirmPublicFunc returns true only after explicit human confirmation.
type ConfirmPublicFunc func(context.Context, PublicConfirmation) (bool, error)

func (a *application) serveCommand() *cobra.Command {
	var (
		route            string
		files            bool
		publicName       string
		wakeOnRequest    bool
		isolate          bool
		yes              bool
		allowCredentials bool
	)
	command := &cobra.Command{
		Use:   "serve",
		Short: "Publish or list services from adopted hosts",
		Example: `  mesh serve HOST TARGET --at ROUTE
  mesh serve ls
  mesh serve list`,
		Args: exactArgs(2, "a host and something to serve", "mesh serve pc ./site --at blog.shaulavo.dev"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runServe(cmd, args[0], args[1], serveFlags{
				route: route, files: files, publicName: publicName,
				wakeOnRequest: wakeOnRequest, isolate: isolate, yes: yes, allowCredentials: allowCredentials,
			})
		},
	}
	command.Flags().StringVar(&route, "at", "", "route path, such as /blog")
	command.Flags().BoolVar(&files, "files", false, "enable directory listings")
	command.Flags().StringVar(&publicName, "public", "", "exact public hostname under shaulavo.dev")
	command.Flags().BoolVar(&wakeOnRequest, "wake-on-request", false, "ask the public edge to wake this origin")
	command.Flags().BoolVar(&isolate, "isolate", false, "send cross-origin isolation headers so the page can use SharedArrayBuffer")
	command.Flags().BoolVar(&yes, "yes", false, "skip the public confirmation prompt")
	command.Flags().BoolVar(&allowCredentials, "allow-credentials", false, "allow credential-like names in a public directory")
	command.AddCommand(a.serveListCommand())
	return command
}

type serveFlags struct {
	route            string
	files            bool
	publicName       string
	wakeOnRequest    bool
	isolate          bool
	yes              bool
	allowCredentials bool
}

func (a *application) runServe(cmd *cobra.Command, hostAlias, target string, flags serveFlags) error {
	name, err := serviceNameFromRoute(flags.route)
	if err != nil {
		return err
	}
	if target == "" {
		return errors.New("TARGET is empty")
	}
	if flags.publicName != "" {
		if err := meshserve.ValidatePublicName(flags.publicName); err != nil {
			return err
		}
	} else {
		switch {
		case flags.allowCredentials:
			return errors.New("--allow-credentials requires --public")
		case flags.wakeOnRequest:
			return errors.New("--wake-on-request requires --public")
		case flags.yes:
			return errors.New("--yes is only meaningful with --public")
		}
	}
	if flags.files && numericCLIServiceTarget(target) {
		return errors.New("--files cannot be combined with a numeric proxy target")
	}
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	host, err := hostWithAlias(hosts, hostAlias)
	if err != nil {
		return err
	}
	kind := ""
	if flags.files {
		kind = string(meshserve.Files)
	}
	requested := protocol.ServiceInfo{
		Name: name, Kind: kind, Target: target, PublicName: flags.publicName, WakeOnRequest: flags.wakeOnRequest,
		Isolate: flags.isolate,
	}
	previewCtx, cancelPreview := context.WithTimeout(cmd.Context(), serviceMutationTimeout)
	preview, privateName, err := previewRemoteService(previewCtx, host, a.dependencies.DialControl, requested, flags.allowCredentials)
	cancelPreview()
	if err != nil {
		return err
	}
	serviceAddress := serviceURL(host, privateName, preview.Service)
	if preview.Service.PublicName != "" {
		confirmation := PublicConfirmation{
			Host: host, Service: preview.Service, FileCount: preview.FileCount,
			URL: serviceAddress, CredentialsOverride: flags.allowCredentials,
		}
		if flags.allowCredentials {
			if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "warning: credential-like entries are explicitly allowed for this publication"); err != nil {
				return err
			}
		}
		if !flags.yes {
			confirmed, err := a.dependencies.ConfirmPublic(cmd.Context(), confirmation)
			if err != nil {
				return err
			}
			if !confirmed {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "publication cancelled")
				return err
			}
		}
	}
	mutationCtx, cancelMutation := context.WithTimeout(cmd.Context(), serviceMutationTimeout)
	persisted, currentPrivateName, err := upsertRemoteService(mutationCtx, host, a.dependencies.DialControl, requested, preview, privateName, flags.allowCredentials)
	cancelMutation()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "serving %s on %s (%s -> %s)\n", serviceURL(host, currentPrivateName, persisted), host.Alias, persisted.Kind, safeTableCell(persisted.Target))
	return err
}

func (a *application) serveListCommand() *cobra.Command {
	var timeout time.Duration
	command := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List live and cached services",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if timeout <= 0 || timeout > maximumServiceListTimeout {
				return fmt.Errorf("--timeout must be between 1ns and %s", maximumServiceListTimeout)
			}
			return a.runServeList(cmd, timeout)
		},
	}
	command.Flags().DurationVar(&timeout, "timeout", defaultServiceListTimeout, "hard deadline for live host and edge queries")
	return command
}

func (a *application) runServeList(cmd *cobra.Command, timeout time.Duration) error {
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	cache, err := OpenCatalogCache(cmd.Context())
	if err != nil {
		return err
	}
	defer cache.Close() //nolint:errcheck // command result takes precedence
	rows, diagnostics, err := CollectServiceCatalog(cmd.Context(), hosts, timeout,
		func(ctx context.Context, host HostRecord) (remoteServiceSnapshot, error) {
			return listRemoteServices(ctx, host, a.dependencies.DialControl)
		},
		func(ctx context.Context, host HostRecord) ([]protocol.EdgeRouteInfo, error) {
			return listRemoteEdge(ctx, host, a.dependencies.DialControl)
		}, cache)
	if err != nil {
		return err
	}
	if err := writeServiceDiagnostics(cmd.ErrOrStderr(), diagnostics); err != nil {
		return err
	}
	writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "ROUTE\tHOST\tKIND\tTARGET\tSCOPE\tHEALTH\tURL"); err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(writer, "/%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			safeTableCell(row.Service.Name), safeTableCell(row.Host.Alias), safeTableCell(row.Service.Kind), safeTableCell(row.Service.Target),
			safeTableCell(row.Scope()), safeTableCell(row.Health()), safeTableCell(row.URL())); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func (a *application) unserveCommand() *cobra.Command {
	var (
		hostAlias string
		timeout   time.Duration
	)
	command := &cobra.Command{
		Use:   "unserve ROUTE",
		Short: "Remove one service and wait for public withdrawal",
		Args:  exactArgs(1, "the route to remove", "mesh unserve blog.shaulavo.dev"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if timeout <= 0 || timeout > maximumServiceListTimeout {
				return fmt.Errorf("--timeout must be between 1ns and %s", maximumServiceListTimeout)
			}
			return a.runUnserve(cmd, args[0], hostAlias, timeout)
		},
	}
	command.Flags().StringVar(&hostAlias, "host", "", "host alias when more than one host owns ROUTE")
	command.Flags().DurationVar(&timeout, "timeout", defaultServiceListTimeout, "hard deadline for ownership discovery")
	return command
}

func (a *application) runUnserve(cmd *cobra.Command, route, hostAlias string, timeout time.Duration) error {
	name, err := serviceNameFromRoute(route)
	if err != nil {
		return err
	}
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	explicitHost := hostAlias != ""
	if explicitHost {
		selected, err := hostWithAlias(hosts, hostAlias)
		if err != nil {
			return err
		}
		hostAlias = selected.Alias
		hosts = []HostRecord{selected}
	}
	cache, err := OpenCatalogCache(cmd.Context())
	if err != nil {
		return err
	}
	defer cache.Close() //nolint:errcheck // command result takes precedence
	rows, diagnostics, err := CollectServiceCatalog(cmd.Context(), hosts, timeout,
		func(ctx context.Context, host HostRecord) (remoteServiceSnapshot, error) {
			return listRemoteServices(ctx, host, a.dependencies.DialControl)
		}, nil, cache)
	if err != nil {
		return err
	}
	candidates := catalogCandidates(rows, name, hostAlias)
	if !explicitHost && len(candidates) <= 1 {
		if aliases := unavailableServiceAliases(diagnostics); len(aliases) > 0 {
			return fmt.Errorf("could not prove /%s has one owner because these hosts are unavailable: %s; choose an owner with --host", name, strings.Join(aliases, ", "))
		}
	}
	if len(candidates) == 0 {
		if explicitHost {
			if aliases := unavailableServiceAliases(diagnostics); len(aliases) > 0 {
				return fmt.Errorf("host %s is unavailable; could not determine whether it serves /%s", hostAlias, name)
			}
			candidates = []ServiceCatalogRow{{
				Host: hosts[0], PrivateName: livePrivateName(rows, hosts[0].ID), Live: true,
			}}
		} else {
			return fmt.Errorf("route /%s is not served by any adopted host", name)
		}
	}
	if len(candidates) > 1 {
		aliases := make([]string, len(candidates))
		for index, candidate := range candidates {
			aliases[index] = candidate.Host.Alias
		}
		sort.Strings(aliases)
		return fmt.Errorf("route /%s is served by multiple hosts (%s); choose one with --host", name, strings.Join(aliases, ", "))
	}
	selected := candidates[0]
	if !selected.Live {
		return fmt.Errorf("host %s is offline; refusing to claim /%s was deleted", selected.Host.Alias, name)
	}
	deleteCtx, cancelDelete := context.WithTimeout(cmd.Context(), serviceMutationTimeout)
	err = deleteRemoteService(deleteCtx, selected.Host, a.dependencies.DialControl, name)
	cancelDelete()
	if err != nil {
		return err
	}
	remaining := make([]protocol.ServiceInfo, 0)
	for _, row := range rows {
		if row.Host.ID == selected.Host.ID && row.Service.Name != name {
			remaining = append(remaining, row.Service)
		}
	}
	cacheCtx, cancelCache := context.WithTimeout(cmd.Context(), 200*time.Millisecond)
	cacheErr := cache.SaveServices(cacheCtx, selected.Host, selected.PrivateName, remaining)
	cancelCache()
	if cacheErr != nil {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "warning: service was deleted but the local cache was not updated (%s)\n", safeRemoteText(cacheErr.Error())); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "unserved /%s on %s\n", name, selected.Host.Alias)
	return err
}

func livePrivateName(rows []ServiceCatalogRow, hostID string) string {
	for _, row := range rows {
		if row.Host.ID == hostID && row.Live {
			return row.PrivateName
		}
	}
	return ""
}

func safeTableCell(value string) string {
	return SafeTerminalText(value)
}

func unavailableServiceAliases(diagnostics map[string]error) []string {
	aliases := make([]string, 0, len(diagnostics))
	for alias, err := range diagnostics {
		var warning catalogCacheWarning
		if errors.As(err, &warning) {
			continue
		}
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	return aliases
}

func serviceNameFromRoute(route string) (string, error) {
	if route == "" {
		return "", errors.New("--at/ROUTE is required")
	}
	if !strings.HasPrefix(route, "/") || strings.HasPrefix(route, "//") {
		return "", fmt.Errorf("route %q must start with exactly one slash", route)
	}
	name := strings.TrimPrefix(route, "/")
	if err := meshserve.ValidateName(name); err != nil {
		return "", err
	}
	if route != "/"+name {
		return "", fmt.Errorf("route %q is not canonical", route)
	}
	return name, nil
}

func serviceURL(host HostRecord, privateName string, service protocol.ServiceInfo) string {
	if service.PublicName != "" {
		return "https://" + service.PublicName + "/" + service.Name
	}
	if privateName != "" {
		return "https://" + privateName + "/" + service.Name
	}
	endpoint, err := url.Parse(host.Endpoint)
	if err != nil || endpoint.User != nil || endpoint.Host == "" || endpoint.RawPath != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "unavailable"
	}
	switch endpoint.Scheme {
	case "ws":
		endpoint.Scheme = "http"
	case "wss":
		endpoint.Scheme = "https"
	default:
		return "unavailable"
	}
	endpoint.Path = "/" + service.Name
	endpoint.RawPath = ""
	endpoint.ForceQuery = false
	return endpoint.String()
}

func numericCLIServiceTarget(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func terminalPublicConfirmation(input *os.File, output io.Writer) ConfirmPublicFunc {
	return func(ctx context.Context, confirmation PublicConfirmation) (bool, error) {
		if ctx == nil {
			return false, errors.New("nil public confirmation context")
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if input == nil || !term.IsTerminal(input.Fd()) {
			return false, errors.New("public publication needs an interactive terminal or --yes")
		}
		if _, err := fmt.Fprintf(output, "Publish this service to the internet?\n  Host: %s\n", confirmation.Host.Alias); err != nil {
			return false, err
		}
		if confirmation.Service.Kind == string(meshserve.Proxy) {
			if _, err := fmt.Fprintf(output, "  Target: port %s\n", safeTableCell(confirmation.Service.Target)); err != nil {
				return false, err
			}
		} else {
			if _, err := fmt.Fprintf(output, "  Resolved path: %s\n  Files: %d\n", safeTableCell(confirmation.Service.Target), confirmation.FileCount); err != nil {
				return false, err
			}
		}
		if _, err := fmt.Fprintf(output, "  URL: %s\n", confirmation.URL); err != nil {
			return false, err
		}
		if confirmation.CredentialsOverride {
			if _, err := fmt.Fprintln(output, "  Credential check: explicitly overridden"); err != nil {
				return false, err
			}
		}
		if _, err := fmt.Fprint(output, "Continue? [y/N] "); err != nil {
			return false, err
		}
		reader, err := cancelreader.NewReader(input)
		if err != nil {
			return false, fmt.Errorf("prepare public confirmation: %w", err)
		}
		defer reader.Close() //nolint:errcheck // prompt result is authoritative
		stopCancellation := context.AfterFunc(ctx, func() { reader.Cancel() })
		answer, err := bufio.NewReader(reader).ReadString('\n')
		stopCancellation()
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if errors.Is(err, cancelreader.ErrCanceled) {
			return false, context.Canceled
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return false, fmt.Errorf("read public confirmation: %w", err)
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		return answer == "y" || answer == "yes", nil
	}
}

func writeServiceDiagnostics(output io.Writer, diagnostics map[string]error) error {
	aliases := make([]string, 0, len(diagnostics))
	for alias := range diagnostics {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		var warning catalogCacheWarning
		if errors.As(diagnostics[alias], &warning) {
			if _, err := fmt.Fprintf(output, "%s: warning (%s)\n", alias, safeRemoteText(warning.Error())); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(output, "%s: unavailable (%s)\n", alias, safeRemoteText(diagnostics[alias].Error())); err != nil {
			return err
		}
	}
	return nil
}
