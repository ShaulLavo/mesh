package serve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultIdle is how long an on-demand route runs with no open
	// connection before its session is stopped.
	DefaultIdle = 15 * time.Minute
	// DefaultReadyTimeout bounds how long a starting route holds connections.
	DefaultReadyTimeout = 60 * time.Second
	// MaximumListens bounds the loopback listeners one route may own.
	MaximumListens = 16
	// MaximumDemandCommandBytes bounds one launch command.
	MaximumDemandCommandBytes = 4_096
	// MaximumDemandEnv bounds the extra environment of one launch recipe.
	MaximumDemandEnv = 64
	// MaximumDemandEnvBytes bounds one KEY=VALUE entry.
	MaximumDemandEnvBytes = 4_096

	minimumDemandDuration = time.Second
	maximumIdle           = 7 * 24 * time.Hour
	maximumReadyTimeout   = time.Hour
)

// Listen binds 127.0.0.1:Public on the origin and proxies it to
// 127.0.0.1:Upstream, so local clients reach a route without the tailnet name.
type Listen struct {
	Public   uint16
	Upstream uint16
}

// Demand is the launch recipe of an on-demand route. The route starts it as a
// session on the first connection and stops it once no connection has been
// open for Idle.
type Demand struct {
	// Command runs through the host user's login shell, so it finds the same
	// tools a terminal on that host would.
	Command      string
	Cwd          string
	Env          []string
	Idle         time.Duration
	ReadyTimeout time.Duration
}

// DemandGate admits requests to on-demand routes. Enter counts one open
// connection to the route until release runs, starting the route's session if
// needed and waiting until its upstreams accept.
type DemandGate interface {
	Enter(ctx context.Context, name string) (release func(), err error)
}

// Equal reports whether two services have the same definition.
func (s Service) Equal(other Service) bool {
	return s.Name == other.Name && s.Kind == other.Kind && s.Target == other.Target &&
		s.PublicName == other.PublicName && s.WakeOnRequest == other.WakeOnRequest &&
		s.Isolate == other.Isolate && s.LocalOnly == other.LocalOnly &&
		slices.Equal(s.Listens, other.Listens) && s.Demand.Equal(other.Demand)
}

// Equal reports whether two recipes are the same; nil equals only nil.
func (d *Demand) Equal(other *Demand) bool {
	if d == nil || other == nil {
		return d == other
	}
	return d.Command == other.Command && d.Cwd == other.Cwd && slices.Equal(d.Env, other.Env) &&
		d.Idle == other.Idle && d.ReadyTimeout == other.ReadyTimeout
}

// Label names the session an on-demand route starts. The daemon finds a
// running session again by it after a restart.
func (s Service) Label() string {
	return "serve " + s.Route()
}

// Route is how people name the service: /name on the tailnet, or :port for a
// route reached only through its local listeners.
func (s Service) Route() string {
	if s.LocalOnly {
		return ":" + s.Name
	}
	return "/" + s.Name
}

// UpstreamPort is the port the tailnet path forwards to. A target that is one
// of the route's own listeners forwards to that listener's upstream instead,
// so the path does not proxy through Mesh twice.
func (s Service) UpstreamPort() string {
	for _, listen := range s.Listens {
		if strconv.Itoa(int(listen.Public)) == s.Target {
			return strconv.Itoa(int(listen.Upstream))
		}
	}
	return s.Target
}

// UpstreamPorts are the ports that must all accept before an on-demand route
// is ready.
func (s Service) UpstreamPorts() []string {
	ports := make([]string, 0, len(s.Listens)+1)
	if !s.LocalOnly {
		ports = append(ports, s.UpstreamPort())
	}
	for _, listen := range s.Listens {
		port := strconv.Itoa(int(listen.Upstream))
		if !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	return ports
}

func normalizeDemand(service Service) (Service, error) {
	if len(service.Listens) == 0 && service.Demand == nil && !service.LocalOnly {
		service.Listens = nil
		return service, nil
	}
	if service.Kind != Proxy {
		return Service{}, fmt.Errorf("serve: service %q: listeners and --run need a proxy port", service.Name)
	}
	if service.LocalOnly && len(service.Listens) == 0 {
		return Service{}, fmt.Errorf("serve: service %q has no tailnet path and no listener", service.Name)
	}
	listens, err := normalizeListens(service.Name, service.Listens)
	if err != nil {
		return Service{}, err
	}
	service.Listens = listens
	if service.LocalOnly && (service.Name != service.Target || !slices.ContainsFunc(listens, func(listen Listen) bool {
		return strconv.Itoa(int(listen.Public)) == service.Target
	})) {
		// The route is addressed as :PORT and reached only there, so the
		// port has to be one Mesh actually listens on.
		return Service{}, fmt.Errorf("serve: local-only route :%s must be named after one of its listener ports", service.Name)
	}
	if service.Demand == nil {
		return service, nil
	}
	if service.PublicName != "" {
		// A public on-demand route would let anyone on the internet start a
		// process here. That is a different exposure from publishing one that
		// already runs, and it needs its own decision.
		return Service{}, fmt.Errorf("serve: service %q: an on-demand route cannot be public", service.Name)
	}
	demand, err := normalizeRecipe(service.Name, *service.Demand)
	if err != nil {
		return Service{}, err
	}
	service.Demand = &demand
	return service, nil
}

func normalizeListens(name string, listens []Listen) ([]Listen, error) {
	if len(listens) == 0 {
		return nil, nil
	}
	if len(listens) > MaximumListens {
		return nil, fmt.Errorf("serve: service %q has more than %d listeners", name, MaximumListens)
	}
	normalized := slices.Clone(listens)
	slices.SortFunc(normalized, func(a, b Listen) int { return int(a.Public) - int(b.Public) })
	upstreams := make(map[uint16]struct{}, len(normalized))
	for _, listen := range normalized {
		upstreams[listen.Upstream] = struct{}{}
	}
	for index, listen := range normalized {
		if listen.Public == 0 || listen.Upstream == 0 {
			return nil, fmt.Errorf("serve: service %q listener %d=%d needs ports from 1 to 65535", name, listen.Public, listen.Upstream)
		}
		if index > 0 && normalized[index-1].Public == listen.Public {
			return nil, fmt.Errorf("serve: service %q listens on port %d twice", name, listen.Public)
		}
		if _, loops := upstreams[listen.Public]; loops {
			return nil, fmt.Errorf("serve: service %q listens on port %d, which is also an upstream", name, listen.Public)
		}
	}
	return normalized, nil
}

func normalizeRecipe(name string, demand Demand) (Demand, error) {
	demand.Command = strings.TrimSpace(demand.Command)
	if demand.Command == "" {
		return Demand{}, fmt.Errorf("serve: service %q has an empty command", name)
	}
	if len(demand.Command) > MaximumDemandCommandBytes || strings.IndexByte(demand.Command, 0) >= 0 {
		return Demand{}, fmt.Errorf("serve: service %q command is longer than %d bytes or holds a null byte", name, MaximumDemandCommandBytes)
	}
	if demand.Cwd == "" || !filepath.IsAbs(demand.Cwd) || strings.IndexByte(demand.Cwd, 0) >= 0 || len(demand.Cwd) > MaximumServiceTargetBytes {
		return Demand{}, fmt.Errorf("serve: service %q working directory %q must be an absolute path", name, demand.Cwd)
	}
	demand.Cwd = filepath.Clean(demand.Cwd)
	if len(demand.Env) > MaximumDemandEnv {
		return Demand{}, fmt.Errorf("serve: service %q has more than %d environment entries", name, MaximumDemandEnv)
	}
	if len(demand.Env) == 0 {
		demand.Env = nil
	} else {
		demand.Env = slices.Clone(demand.Env)
	}
	for _, entry := range demand.Env {
		if err := validateEnvEntry(entry); err != nil {
			return Demand{}, fmt.Errorf("serve: service %q: %w", name, err)
		}
	}
	if demand.Idle == 0 {
		demand.Idle = DefaultIdle
	}
	if demand.ReadyTimeout == 0 {
		demand.ReadyTimeout = DefaultReadyTimeout
	}
	if demand.Idle < minimumDemandDuration || demand.Idle > maximumIdle {
		return Demand{}, fmt.Errorf("serve: service %q idle window %s is outside %s to %s", name, demand.Idle, minimumDemandDuration, maximumIdle)
	}
	if demand.ReadyTimeout < minimumDemandDuration || demand.ReadyTimeout > maximumReadyTimeout {
		return Demand{}, fmt.Errorf("serve: service %q ready timeout %s is outside %s to %s", name, demand.ReadyTimeout, minimumDemandDuration, maximumReadyTimeout)
	}
	// Durations travel as milliseconds, so anything finer would not survive
	// a round trip and the definition would never compare equal.
	demand.Idle = demand.Idle.Truncate(time.Millisecond)
	demand.ReadyTimeout = demand.ReadyTimeout.Truncate(time.Millisecond)
	return demand, nil
}

// ValidateEnvEntry checks one KEY=VALUE entry of a launch recipe.
func ValidateEnvEntry(entry string) error {
	return validateEnvEntry(entry)
}

func validateEnvEntry(entry string) error {
	key, _, found := strings.Cut(entry, "=")
	if !found || key == "" {
		return fmt.Errorf("environment entry %q is not KEY=VALUE", entry)
	}
	if len(entry) > MaximumDemandEnvBytes || strings.IndexByte(entry, 0) >= 0 {
		return fmt.Errorf("environment entry %s is longer than %d bytes or holds a null byte", key, MaximumDemandEnvBytes)
	}
	for index, character := range key {
		letter := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character == '_'
		if !letter && (index == 0 || character < '0' || character > '9') {
			return fmt.Errorf("environment name %q is not a valid variable name", key)
		}
	}
	return nil
}

// gatedHandler holds a request to an on-demand route open until the route is
// ready, and counts it as a connection for as long as it lasts. An upgraded
// WebSocket keeps the proxy's ServeHTTP running, so it counts until closed.
func (r *Registry) gatedHandler(name string, inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		gate := r.gate.Load()
		if gate == nil {
			http.Error(w, "on-demand serving is not available", http.StatusServiceUnavailable)
			return
		}
		release, err := (*gate).Enter(request.Context(), name)
		if err != nil {
			WriteDemandFailure(w, err)
			return
		}
		defer release()
		inner.ServeHTTP(w, request)
	})
}

// SetDemandGate installs the gate that on-demand routes wait on. Until it is
// set they answer 503, which only happens while the daemon is starting.
func (r *Registry) SetDemandGate(gate DemandGate) {
	r.gate.Store(&gate)
}

// WriteDemandFailure answers a request an on-demand route could not admit.
// The body is plain text because the one reading it is usually a person
// wondering why their dev server did not come up.
func WriteDemandFailure(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, err.Error())
}
