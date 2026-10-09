package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/privacy"
	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/spf13/cobra"
)

const (
	defaultServiceListTimeout = 2 * time.Second
	maximumServiceListTimeout = 30 * time.Second
	serviceMutationTimeout    = 30 * time.Second
)

func (a *application) serveCommand() *cobra.Command {
	var (
		route       string
		displayName string
		files       bool

		privateHost string

		isolate bool

		run          string
		cwd          string
		env          []string
		listens      []string
		idle         time.Duration
		readyTimeout time.Duration
	)
	command := &cobra.Command{
		Use:   "serve",
		Short: "Publish or list services from adopted hosts",
		Example: `  mesh serve HOST TARGET --at ROUTE
  mesh serve pc 5173 --at /dev --run 'bun run dev' --listen 5173=15173
  mesh serve pc --run 'bun run dev' --listen 5173=15173 --listen 3001=13001
  mesh serve ls
  mesh serve list
  mesh serve start ROUTE
  mesh serve stop ROUTE`,
		Args: func(cmd *cobra.Command, args []string) error {
			// A route reached only through its listeners needs no TARGET:
			// its first --listen port stands in for one.
			if len(args) == 1 && len(listens) > 0 {
				return nil
			}
			return exactArgs(2, "a host and something to serve", "mesh serve pc ./site --at blog.mesh.test")(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 2 {
				target = args[1]
			} else if port, local := strings.CutPrefix(route, ":"); local {
				target = port
			} else if first, err := parseListen(listens[0]); err == nil {
				target = strconv.Itoa(first.Public)
			} else {
				return err
			}
			return a.runServe(cmd, args[0], target, serveFlags{
				route: route, displayName: displayName, files: files, privateHost: privateHost, privateHostSet: cmd.Flags().Changed("private-host"), isolate: isolate,
				run: run, cwd: cwd, env: env, listens: listens, idle: idle, readyTimeout: readyTimeout,
				cwdSet: cmd.Flags().Changed("cwd"), idleSet: cmd.Flags().Changed("idle"),
				readySet: cmd.Flags().Changed("ready-timeout"),
			})
		},
	}
	command.Flags().StringVar(&displayName, "label", "", "display name shown in service lists")
	command.Flags().StringVar(&route, "at", "", "route path, such as /blog")
	command.Flags().BoolVar(&files, "files", false, "enable directory listings")
	command.Flags().StringVar(&privateHost, "private-host", "", "private hostname at the root, using a label or a configured deployment hostname")

	command.Flags().BoolVar(&isolate, "isolate", false, "send cross-origin isolation headers so the page can use SharedArrayBuffer")

	command.Flags().StringVar(&run, "run", "", "command that serves the port; started on the first connection, stopped when idle")
	command.Flags().StringVar(&cwd, "cwd", "", "directory --run starts in (default: this directory)")
	command.Flags().StringArrayVar(&env, "env", nil, "KEY=VALUE added to the environment of --run (repeatable)")
	command.Flags().StringArrayVar(&listens, "listen", nil, "PUBLIC=UPSTREAM: proxy 127.0.0.1:PUBLIC on the host to 127.0.0.1:UPSTREAM (repeatable)")
	command.Flags().DurationVar(&idle, "idle", meshserve.DefaultIdle, "stop --run after no connection has been open this long")
	command.Flags().DurationVar(&readyTimeout, "ready-timeout", meshserve.DefaultReadyTimeout, "how long a starting --run holds connections before failing them")
	command.AddCommand(a.serveListCommand(), a.serveLabelCommand(), a.serveStartStopCommand(true), a.serveStartStopCommand(false))

	return command
}

type serveFlags struct {
	privateHostSet bool
	route          string
	displayName    string
	files          bool

	privateHost string

	isolate bool

	run          string
	cwd          string
	env          []string
	listens      []string
	idle         time.Duration
	readyTimeout time.Duration
	cwdSet       bool
	idleSet      bool
	readySet     bool
}

func (a *application) runServe(cmd *cobra.Command, hostID, target string, flags serveFlags) error {
	if target == "" {
		return errors.New("TARGET is empty")
	}
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	host, err := resolveHostTarget(hosts, hostID)
	if err != nil {
		return err
	}
	demand, err := serveDemandFromFlags(target, flags, isThisHost(host))
	if err != nil {
		return err
	}
	flags.privateHost = strings.ToLower(flags.privateHost)
	if flags.privateHost != "" && domainpolicy.Primary() == "" {
		return errors.New("--private-host requires a deployment domain in domains.json")
	}
	if flags.privateHost != "" && !strings.Contains(flags.privateHost, ".") {
		flags.privateHost += "." + domainpolicy.Primary()
	}
	if flags.privateHost != "" && flags.route == "" {
		label, _, _ := domainpolicy.Label(flags.privateHost, false)
		flags.route = "/" + label
	}
	name, localOnly, err := serveRouteName(flags.route, target, demand.listens)
	if err != nil {
		return err
	}

	if flags.files && numericCLIServiceTarget(target) {
		return errors.New("--files cannot be combined with a numeric proxy target")
	}
	kind := ""
	if flags.files {
		kind = string(meshserve.Files)
	}
	requested := protocol.ServiceInfo{
		DisplayName: flags.displayName,
		Name:        name, Kind: kind, Target: target, PrivateHost: flags.privateHost,
		Isolate: flags.isolate, Listens: demand.listens, Run: demand.run, LocalOnly: localOnly,
	}
	if !flags.privateHostSet && flags.privateHost == "" {
		inherited, err := a.existingPrivateHost(cmd.Context(), host, name)
		if err != nil {
			return err
		}
		requested.PrivateHost = inherited
	}
	previewCtx, cancelPreview := context.WithTimeout(cmd.Context(), serviceMutationTimeout)
	preview, privateName, err := previewRemoteService(previewCtx, host, a.dependencies.DialControl, requested)
	cancelPreview()
	if err != nil {
		return err
	}

	mutationCtx, cancelMutation := context.WithTimeout(cmd.Context(), serviceMutationTimeout)
	publication, err := upsertRemoteService(mutationCtx, host, a.dependencies.DialControl, requested, preview, privateName)
	cancelMutation()
	if err != nil {
		return err
	}
	if publication.Warning != "" {
		for warning := range strings.SplitSeq(publication.Warning, "\n") {
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s: %s\n", safeTableCell(a.privacy.Value("host", HostLabel(host))), serviceDiagnostic(a.privacy, warning)); err != nil {
				return fmt.Errorf("write service shadow warning: %w", err)
			}
		}
	}
	persisted := publication.Service
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "serving %s on %s (%s -> %s)\n", a.privacy.Value("url", serviceURL(host, publication.PrivateName, persisted)), a.privacy.Value("host", HostLabel(host)), persisted.Kind, privateServiceTarget(a.privacy, persisted))
	if err != nil || persisted.Run == nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "starts %q in %s on the first connection; stops after %s idle\n",
		privateServiceCommand(a.privacy, persisted.Run.Command), safeTableCell(a.privacy.Value("path", persisted.Run.Cwd)), time.Duration(persisted.Run.IdleMillis)*time.Millisecond)
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
	command.Flags().DurationVar(&timeout, "timeout", defaultServiceListTimeout, "hard deadline for live host queries")
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

		cache)
	if err != nil {
		return err
	}
	if err := writeServiceDiagnostics(cmd.ErrOrStderr(), diagnostics, a.privacy); err != nil {
		return err
	}
	return writeServiceTable(cmd.OutOrStdout(), rows, a.privacy)
}

func (a *application) unserveCommand() *cobra.Command {
	var (
		hostID string

		timeout time.Duration
	)
	command := &cobra.Command{
		Use:   "unserve ROUTE",
		Short: "Remove one private service",
		Args:  exactArgs(1, "the route to remove", "mesh unserve blog.mesh.test"),
		RunE: func(cmd *cobra.Command, args []string) error {

			if timeout <= 0 || timeout > maximumServiceListTimeout {
				return fmt.Errorf("--timeout must be between 1ns and %s", maximumServiceListTimeout)
			}
			return a.runUnserve(cmd, args[0], hostID, timeout)
		},
	}
	command.Flags().StringVar(&hostID, "host", "", "machine name or exact host ID when more than one host owns ROUTE")

	command.Flags().DurationVar(&timeout, "timeout", defaultServiceListTimeout, "hard deadline for ownership discovery")
	return command
}

func (a *application) runUnserve(cmd *cobra.Command, route, hostID string, timeout time.Duration) error {
	name, err := serviceNameFromRoute(route)
	if err != nil {
		return err
	}
	cache, err := OpenCatalogCache(cmd.Context())
	if err != nil {
		return err
	}
	defer cache.Close() //nolint:errcheck // command result takes precedence
	selected, rows, err := a.resolveServiceOwner(cmd, cache, route, name, hostID, timeout)
	if err != nil {
		return err
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
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "warning: service was deleted but the local cache was not updated (%s)\n", serviceDiagnostic(a.privacy, cacheErr.Error())); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "unserved %s on %s\n", a.privacy.Value("route", route), a.privacy.Value("host", HostLabel(selected.Host)))
	return err
}

// resolveServiceOwner finds the one live host that serves name. It refuses
// when several hosts serve it, or when an unavailable host makes that
// impossible to rule out, unless --host names the owner.
func (a *application) resolveServiceOwner(cmd *cobra.Command, cache *SQLiteCatalogCache, route, name, hostID string, timeout time.Duration) (ServiceCatalogRow, []ServiceCatalogRow, error) {
	hosts, err := LoadHosts()
	if err != nil {
		return ServiceCatalogRow{}, nil, err
	}
	explicitHost := hostID != ""
	if explicitHost {
		selected, err := resolveHostTarget(hosts, hostID)
		if err != nil {
			return ServiceCatalogRow{}, nil, err
		}
		hostID = selected.ID
		hosts = []HostRecord{selected}
	}
	rows, diagnostics, err := CollectServiceCatalog(cmd.Context(), hosts, timeout,
		func(ctx context.Context, host HostRecord) (remoteServiceSnapshot, error) {
			return listRemoteServices(ctx, host, a.dependencies.DialControl)
		}, cache)
	if err != nil {
		return ServiceCatalogRow{}, nil, err
	}
	candidates := catalogCandidates(rows, name, hostID)
	if !explicitHost && len(candidates) <= 1 {
		if aliases := unavailableServiceAliases(diagnostics); len(aliases) > 0 {
			return ServiceCatalogRow{}, nil, fmt.Errorf("could not prove %s has one owner because these hosts are unavailable: %s; choose an owner with --host", route, strings.Join(aliases, ", "))
		}
	}
	if len(candidates) == 0 {
		if explicitHost {
			if aliases := unavailableServiceAliases(diagnostics); len(aliases) > 0 {
				return ServiceCatalogRow{}, nil, fmt.Errorf("host %s is unavailable; could not determine whether it serves %s", hostID, route)
			}
			candidates = []ServiceCatalogRow{{
				Host: hosts[0], PrivateName: livePrivateName(rows, hosts[0].ID), Live: true,
			}}
		} else {
			return ServiceCatalogRow{}, nil, fmt.Errorf("route %s is not served by any adopted host", route)
		}
	}
	if len(candidates) > 1 {
		aliases := make([]string, len(candidates))
		for index, candidate := range candidates {
			aliases[index] = HostLabel(candidate.Host)
		}
		sort.Strings(aliases)
		return ServiceCatalogRow{}, nil, fmt.Errorf("route %s is served by multiple hosts (%s); choose one with --host", route, strings.Join(aliases, ", "))
	}
	selected := candidates[0]
	if !selected.Live {
		return ServiceCatalogRow{}, nil, fmt.Errorf("host %s is offline; refusing to act on %s", HostLabel(selected.Host), route)
	}
	return selected, rows, nil
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
	if port, local := strings.CutPrefix(route, ":"); local {
		// A route reached only through its listener is named by that port.
		if parsed, err := strconv.ParseUint(port, 10, 16); err != nil || parsed == 0 || strconv.FormatUint(parsed, 10) != port {
			return "", fmt.Errorf("route %q is not :PORT", route)
		}
		return port, nil
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
	if service.LocalOnly {
		// Reachable only from the host itself.
		return "http://127.0.0.1:" + service.Name + "/"
	}
	if service.PrivateHost != "" {
		return "https://" + service.PrivateHost + "/"
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

func writeServiceDiagnostics(output io.Writer, diagnostics map[string]error, masks ...*privacy.Mask) error {
	mask := presentationMask(masks)
	aliases := make([]string, 0, len(diagnostics))
	for alias := range diagnostics {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		var warning catalogCacheWarning
		if errors.As(diagnostics[alias], &warning) {
			if _, err := fmt.Fprintf(output, "%s: warning (%s)\n", mask.Value("host", alias), serviceDiagnostic(mask, warning.Error())); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(output, "%s: unavailable (%s)\n", mask.Value("host", alias), serviceDiagnostic(mask, diagnostics[alias].Error())); err != nil {
			return err
		}
	}
	return nil
}

func writeServiceTable(output io.Writer, rows []ServiceCatalogRow, mask *privacy.Mask) error {
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "ROUTE\tNAME\tHOST\tKIND\tTARGET\tSCOPE\tSTATE\tHEALTH\tURL"); err != nil {
		return fmt.Errorf("write service table: %w", err)
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			safeTableCell(mask.Value("route", serviceRoute(row.Service))), safeTableCell(mask.Value("name", serviceDisplayName(row.Service))), safeTableCell(mask.Value("host", HostLabel(row.Host))), safeTableCell(row.Service.Kind), privateServiceTarget(mask, row.Service),
			safeTableCell(row.Scope()), safeTableCell(row.State()), safeTableCell(row.Health()), safeTableCell(mask.Value("url", row.URL()))); err != nil {
			return fmt.Errorf("write service table: %w", err)
		}
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("flush service table: %w", err)
	}
	return nil
}

func privateServiceTarget(mask *privacy.Mask, service protocol.ServiceInfo) string {
	if mask == nil || numericCLIServiceTarget(service.Target) {
		return serviceTargetCell(service)
	}
	// Copy the presentation value, leaving the authoritative route untouched.
	service.Target = mask.Value("path", service.Target)
	return serviceTargetCell(service)
}

func privateServiceCommand(mask *privacy.Mask, command string) string {
	if mask != nil {
		return "[command withheld]"
	}
	return command
}

func serviceDiagnostic(mask *privacy.Mask, text string) string {
	if mask != nil {
		return "[details withheld]"
	}
	return safeRemoteText(text)
}
