package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

// serviceStartMargin is added to a route's ready timeout when the CLI waits
// for service.start, so the daemon's own timeout is the one that answers.
const serviceStartMargin = 30 * time.Second

type serveDemand struct {
	listens []protocol.ServiceListen
	run     *protocol.ServiceRun
}

// serveRouteName decides what a route is called and whether it has a tailnet
// path. A route with listeners and no --at, or with --at :PORT, is reached
// only on the host, and is named after the listener port it is reached on.
func serveRouteName(route, target string, listens []protocol.ServiceListen) (string, bool, error) {
	if route != "" && !strings.HasPrefix(route, ":") {
		name, err := serviceNameFromRoute(route)
		return name, false, err
	}
	if route == "" && len(listens) == 0 {
		_, err := serviceNameFromRoute(route)
		return "", false, err
	}
	name := target
	if route != "" {
		var err error
		if name, err = serviceNameFromRoute(route); err != nil {
			return "", false, err
		}
		if name != target {
			return "", false, fmt.Errorf("route %s is reached on port %s, so TARGET must be %s", route, name, name)
		}
	}
	if !slices.ContainsFunc(listens, func(listen protocol.ServiceListen) bool { return strconv.Itoa(listen.Public) == name }) {
		return "", false, fmt.Errorf("route :%s has no tailnet path, so it needs --listen %s=UPSTREAM (or pass --at /ROUTE)", name, name)
	}
	return name, true, nil
}

// isThisHost reports whether host is the machine the CLI runs on, which is
// the only place the CLI's own working directory means anything.
func isThisHost(host HostRecord) bool {
	stateDir, err := paths.StateDir()
	if err != nil {
		return false
	}
	self, err := identity.Load(stateDir)
	return err == nil && self.ID == host.ID
}

func serveDemandFromFlags(target string, flags serveFlags, localHost bool) (serveDemand, error) {
	var demand serveDemand
	for _, value := range flags.listens {
		listen, err := parseListen(value)
		if err != nil {
			return serveDemand{}, err
		}
		demand.listens = append(demand.listens, listen)
	}
	if flags.run == "" {
		switch {
		case flags.cwdSet:
			return serveDemand{}, errors.New("--cwd needs --run")
		case len(flags.env) > 0:
			return serveDemand{}, errors.New("--env needs --run")
		case flags.idleSet:
			return serveDemand{}, errors.New("--idle needs --run")
		case flags.readySet:
			return serveDemand{}, errors.New("--ready-timeout needs --run")
		}
	}
	if flags.run == "" && len(demand.listens) == 0 {
		return demand, nil
	}
	if !numericCLIServiceTarget(target) {
		return serveDemand{}, errors.New("--run and --listen need a port as TARGET")
	}
	if flags.files {
		return serveDemand{}, errors.New("--files cannot be combined with --run or --listen")
	}
	if flags.run == "" {
		return demand, nil
	}
	if flags.publicName != "" {
		return serveDemand{}, errors.New("--run cannot be combined with --public: a request from the internet would start a process on the host")
	}
	cwd := flags.cwd
	switch {
	case !flags.cwdSet && !localHost:
		return serveDemand{}, errors.New("--run on another machine needs --cwd: this directory is on this machine, not there")
	case !flags.cwdSet:
		var err error
		if cwd, err = os.Getwd(); err != nil {
			return serveDemand{}, fmt.Errorf("find the directory for --run: %w", err)
		}
	case cwd == "":
		return serveDemand{}, errors.New("--cwd is empty")
	case !filepath.IsAbs(cwd) && localHost:
		absolute, err := filepath.Abs(cwd)
		if err != nil {
			return serveDemand{}, fmt.Errorf("--cwd %s: %w", cwd, err)
		}
		cwd = absolute
	}
	// A relative --cwd for another machine is sent as typed; that host
	// resolves it against its own home, as it does a directory TARGET.
	for _, entry := range flags.env {
		if err := meshserve.ValidateEnvEntry(entry); err != nil {
			return serveDemand{}, fmt.Errorf("--env: %w", err)
		}
	}
	if flags.idle < time.Second || flags.readyTimeout < time.Second {
		return serveDemand{}, errors.New("--idle and --ready-timeout must be at least 1s")
	}
	demand.run = &protocol.ServiceRun{
		Command: strings.TrimSpace(flags.run), Cwd: filepath.Clean(cwd), Env: slices.Clone(flags.env),
		IdleMillis: flags.idle.Milliseconds(), ReadyTimeoutMillis: flags.readyTimeout.Milliseconds(),
	}
	return demand, nil
}

func parseListen(value string) (protocol.ServiceListen, error) {
	public, upstream, found := strings.Cut(value, "=")
	publicPort, publicErr := strconv.ParseUint(public, 10, 16)
	upstreamPort, upstreamErr := strconv.ParseUint(upstream, 10, 16)
	if !found || publicErr != nil || upstreamErr != nil || publicPort == 0 || upstreamPort == 0 {
		return protocol.ServiceListen{}, fmt.Errorf("--listen %q is not PUBLIC=UPSTREAM with two ports", value)
	}
	if publicPort == upstreamPort {
		return protocol.ServiceListen{}, fmt.Errorf("--listen %q proxies a port to itself", value)
	}
	return protocol.ServiceListen{Public: int(publicPort), Upstream: int(upstreamPort)}, nil
}

// sameDemandDefinition compares the on-demand half of two definitions after
// normalization, which sorts listeners and fills default durations.
func sameDemandDefinition(requested, returned protocol.ServiceInfo) bool {
	want, got := protocol.ServiceFromInfo(requested), protocol.ServiceFromInfo(returned)
	if want.Demand == nil && len(want.Listens) == 0 && !want.LocalOnly {
		return got.Demand == nil && len(got.Listens) == 0 && !got.LocalOnly
	}
	// The requested kind may still be empty for the daemon to infer; these
	// fields exist only on a proxy.
	want.Kind = meshserve.Proxy
	if want.Demand != nil && got.Demand != nil && !filepath.IsAbs(want.Demand.Cwd) {
		// The host resolved a relative --cwd against its own home.
		if !strings.HasSuffix(got.Demand.Cwd, string(filepath.Separator)+want.Demand.Cwd) {
			return false
		}
		want.Demand.Cwd = got.Demand.Cwd
	}
	want, err := meshserve.Normalize(want)
	if err != nil {
		return false
	}
	return want.LocalOnly == got.LocalOnly && slices.Equal(want.Listens, got.Listens) && want.Demand.Equal(got.Demand)
}

func serviceRoute(service protocol.ServiceInfo) string {
	if service.LocalOnly {
		return ":" + service.Name
	}
	return "/" + service.Name
}

// serviceTargetCell shows where a route forwards: its port, and each local
// listener as PUBLIC→UPSTREAM.
func serviceTargetCell(service protocol.ServiceInfo) string {
	if len(service.Listens) == 0 {
		return safeTableCell(service.Target)
	}
	parts := make([]string, 0, len(service.Listens)+1)
	if !service.LocalOnly && !slices.ContainsFunc(service.Listens, func(listen protocol.ServiceListen) bool {
		return strconv.Itoa(listen.Public) == service.Target
	}) {
		parts = append(parts, safeTableCell(service.Target))
	}
	for _, listen := range service.Listens {
		parts = append(parts, fmt.Sprintf(":%d→%d", listen.Public, listen.Upstream))
	}
	return strings.Join(parts, " ")
}

// State is what an on-demand route's session is doing; "-" for anything else.
func (r ServiceCatalogRow) State() string {
	if !r.Live || r.Service.Run == nil || r.Service.Demand == nil || r.Service.Demand.State == "" {
		return "-"
	}
	demand := r.Service.Demand
	if demand.State != protocol.DemandRunning || demand.Connections == 0 {
		return demand.State
	}
	if demand.Connections == 1 {
		return "running (1 conn)"
	}
	return fmt.Sprintf("running (%d conns)", demand.Connections)
}

// demandHealth is the HEALTH of an on-demand route that is not running: its
// state, because a stopped route is waiting for a connection, not broken.
func (r ServiceCatalogRow) demandHealth() (string, bool) {
	if !r.Live || r.Service.Run == nil || r.Service.Demand == nil {
		return "", false
	}
	switch r.Service.Demand.State {
	case protocol.DemandStopped, protocol.DemandStarting, protocol.DemandStopping, protocol.DemandFailed:
		return r.Service.Demand.State, true
	}
	return "", false
}

func validateServiceDemand(demand *protocol.ServiceDemand) error {
	if demand == nil {
		return nil
	}
	switch demand.State {
	case "", protocol.DemandStopped, protocol.DemandStarting, protocol.DemandRunning, protocol.DemandStopping, protocol.DemandFailed:
	default:
		return errors.New("service demand state is invalid")
	}
	if demand.Connections < 0 || len(demand.Unbound) > meshserve.MaximumListens || len(demand.SessionID) > 64 {
		return errors.New("service demand status is invalid")
	}
	for _, text := range append([]string{demand.Failure}, demand.Unbound...) {
		if len(text) > meshserve.MaximumServiceProblemBytes || !utf8.ValidString(text) {
			return errors.New("service demand status is invalid")
		}
	}
	return nil
}

func (a *application) serveStartStopCommand(start bool) *cobra.Command {
	var (
		hostAlias string
		timeout   time.Duration
	)
	use, short := "stop ROUTE", "Stop an on-demand route now; the next connection starts it again"
	if start {
		use, short = "start ROUTE", "Start an on-demand route now and wait until it is ready"
	}
	command := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  exactArgs(1, "the route", "mesh serve "+strings.Fields(use)[0]+" /dev"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if timeout <= 0 || timeout > maximumServiceListTimeout {
				return fmt.Errorf("--timeout must be between 1ns and %s", maximumServiceListTimeout)
			}
			return a.runServeStartStop(cmd, args[0], hostAlias, timeout, start)
		},
	}
	command.Flags().StringVar(&hostAlias, "host", "", "host alias when more than one host owns ROUTE")
	command.Flags().DurationVar(&timeout, "timeout", defaultServiceListTimeout, "hard deadline for ownership discovery")
	return command
}

func (a *application) runServeStartStop(cmd *cobra.Command, route, hostAlias string, timeout time.Duration, start bool) error {
	name, err := serviceNameFromRoute(route)
	if err != nil {
		return err
	}
	cache, err := OpenCatalogCache(cmd.Context())
	if err != nil {
		return err
	}
	defer cache.Close() //nolint:errcheck // command result takes precedence
	selected, _, err := a.resolveServiceOwner(cmd, cache, route, name, hostAlias, timeout)
	if err != nil {
		return err
	}
	if selected.Service.Name != "" && selected.Service.Run == nil {
		return fmt.Errorf("route %s on %s has no --run command", route, selected.Host.Alias)
	}
	requestType, operation, wait := protocol.TypeServiceStop, "service stop", serviceMutationTimeout
	if start {
		requestType, operation = protocol.TypeServiceStart, "service start"
		if selected.Service.Run != nil {
			wait = time.Duration(selected.Service.Run.ReadyTimeoutMillis)*time.Millisecond + serviceStartMargin
		}
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), wait)
	defer cancel()
	response, _, err := remoteServiceRequest(ctx, selected.Host, a.dependencies.DialControl, protocol.Control{Type: requestType, ServiceName: name}, nil)
	if err != nil {
		return err
	}
	if response.Type == protocol.TypeError {
		return remoteServiceResponseError(selected.Host, operation, response)
	}
	if response.Type != protocol.TypeOK || response.ServiceName != name || response.Service == nil {
		return fmt.Errorf("host %s returned an invalid %s acknowledgement", selected.Host.Alias, operation)
	}
	status, err := validateRemoteService(*response.Service)
	if err != nil {
		return err
	}
	row := ServiceCatalogRow{Host: selected.Host, Service: status, Live: true}
	state := row.State()
	if status.Demand != nil && status.Demand.SessionID != "" && state != protocol.DemandStopped {
		state += ", session " + safeTableCell(status.Demand.SessionID)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s on %s: %s\n", route, selected.Host.Alias, state)
	return err
}
