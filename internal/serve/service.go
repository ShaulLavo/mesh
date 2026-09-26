// Package serve exposes origin-side HTTP services and path confinement.
package serve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// ReservedPrefix is the default Mesh WebSocket protocol path. Service routes
	// may not overlap the configured protocol path.
	ReservedPrefix = "/mesh"
	// PublicDomain is the only public DNS zone Mesh routes may name.
	PublicDomain = "shaulavo.dev"
	// MaximumServices bounds one origin snapshot and its service.list frame.
	MaximumServices = 256
	// MaximumServiceNameBytes bounds a canonical route name.
	MaximumServiceNameBytes = 512
	// MaximumServiceTargetBytes bounds a directory path carried over control.
	MaximumServiceTargetBytes = 2_048
	// MaximumServiceProblemBytes bounds one cached health diagnostic.
	MaximumServiceProblemBytes = 256

	// upstreamProbeTimeout bounds one proxy health dial. Loopback refuses or
	// accepts at once, so only a wedged listener ever reaches it.
	upstreamProbeTimeout = 300 * time.Millisecond
)

// Kind identifies what an origin service exposes.
type Kind string

const (
	Static Kind = "static"
	Files  Kind = "files"
	Proxy  Kind = "proxy"
)

// Service is one durable route on an origin host.
type Service struct {
	Name          string
	Kind          Kind
	Target        string
	PublicName    string
	WakeOnRequest bool
	// Isolate sends the cross-origin isolation headers with every response,
	// which is what browsers require before they enable SharedArrayBuffer.
	// Off by default because the embedder policy also blocks cross-origin
	// subresources that do not opt in.
	Isolate bool
	// Listens are loopback ports the origin binds for this route. Only a
	// proxy route has them.
	Listens []Listen
	// Demand makes a proxy route start its upstream on the first connection
	// and stop it when idle. Nil for a route to something already running.
	Demand *Demand
	// LocalOnly routes have no tailnet path; Name is then the route's first
	// listener port and exists only to address the route.
	LocalOnly bool
}

// ServiceStatus reports whether the service target is available now.
type ServiceStatus struct {
	Service Service
	Healthy bool
	Problem string
}

// Registry routes requests to a complete service snapshot. Replace publishes a
// new snapshot atomically, so in-flight requests keep using the old handlers.
type Registry struct {
	reservedPrefix        string
	trustForwardedHeaders func(netip.Addr) bool
	snapshot              atomic.Pointer[registrySnapshot]
	gate                  atomic.Pointer[DemandGate]
}

type registrySnapshot struct {
	services []Service
	routes   []serviceRoute
}

type serviceRoute struct {
	prefix  string
	handler http.Handler
}

// Normalize validates service and resolves directory targets to absolute paths.
func Normalize(service Service) (Service, error) {
	return normalizeService(service)
}

// ValidateName checks one service route name without requiring a target.
func ValidateName(name string) error {
	return validateRouteName(name)
}

// ValidatePublicName checks that name is empty or exactly one canonical DNS
// label below the public Mesh zone.
func ValidatePublicName(name string) error {
	return validatePublicName(name)
}

// ReservedPrefix returns the protocol path this registry keeps free of services.
func (r *Registry) ReservedPrefix() string {
	return r.reservedPrefix
}

// NewRegistry builds a registry from one complete service list.
func NewRegistry(services []Service) (*Registry, error) {
	return NewRegistryWithReservedPrefix(services, ReservedPrefix, nil)
}

// NewRegistryWithReservedPrefix builds a registry that refuses any service
// route overlapping the daemon protocol path. trustForwardedHeaders may trust
// canonical forwarding metadata only from a separately authenticated peer.
func NewRegistryWithReservedPrefix(services []Service, reservedPrefix string, trustForwardedHeaders func(netip.Addr) bool) (*Registry, error) {
	if err := validatePrefix(reservedPrefix); err != nil {
		return nil, fmt.Errorf("serve: invalid reserved prefix: %w", err)
	}
	registry := &Registry{reservedPrefix: reservedPrefix, trustForwardedHeaders: trustForwardedHeaders}
	if err := registry.Replace(services); err != nil {
		return nil, err
	}
	return registry, nil
}

// Replace validates and atomically publishes a complete service list.
func (r *Registry) Replace(services []Service) error {
	snapshot, err := r.buildSnapshot(services)
	if err != nil {
		return err
	}
	r.snapshot.Store(snapshot)
	return nil
}

// Services returns the current service definitions in route-name order.
func (r *Registry) Services() []Service {
	snapshot := r.snapshot.Load()
	if snapshot == nil {
		return []Service{}
	}
	return append([]Service(nil), snapshot.services...)
}

// CheckServices probes every service concurrently and returns statuses in
// input order. Callers hold no lock across it: a proxy probe may take up to
// upstreamProbeTimeout, and MaximumServices bounds the fan-out.
func CheckServices(ctx context.Context, services []Service) []ServiceStatus {
	statuses := make([]ServiceStatus, len(services))
	var group sync.WaitGroup
	for index, service := range services {
		group.Go(func() { statuses[index] = CheckService(ctx, service) })
	}
	group.Wait()
	return statuses
}

// CheckService reports whether a request routed to service could be answered
// now: a directory root must open, and a proxy upstream must accept a
// connection on the address its handler forwards to.
func CheckService(ctx context.Context, service Service) ServiceStatus {
	var err error
	switch service.Kind {
	case Static, Files:
		err = checkRoot(service.Target)
	case Proxy:
		err = checkUpstream(ctx, upstreamAddress(service.UpstreamPort()))
	}
	if err != nil {
		return ServiceStatus{Service: service, Problem: err.Error()}
	}
	return ServiceStatus{Service: service, Healthy: true}
}

func checkRoot(target string) error {
	file, _, err := OpenRootEntry(target, "/")
	if err != nil {
		return err
	}
	_ = file.Close()
	return nil
}

func checkUpstream(ctx context.Context, address string) error {
	ctx, cancel := context.WithTimeout(ctx, upstreamProbeTimeout)
	defer cancel()
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "tcp", address)
	var operation *net.OpError
	if errors.As(err, &operation) && operation.Err != nil {
		// The dial error repeats the address; keep only why it failed.
		return fmt.Errorf("upstream %s unreachable: %w", address, operation.Err)
	}
	if err != nil {
		return fmt.Errorf("upstream %s unreachable: %w", address, err)
	}
	_ = connection.Close()
	return nil
}

// upstreamAddress is the one place a proxy port becomes a dial address, so the
// health probe cannot drift from where proxyHandler actually forwards.
func upstreamAddress(port string) string {
	return net.JoinHostPort("127.0.0.1", port)
}

// ServeHTTP dispatches by longest path prefix and returns 404 for unknown paths.
func (r *Registry) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	snapshot := r.snapshot.Load()
	if snapshot == nil {
		http.NotFound(w, request)
		return
	}
	requestPath := request.URL.EscapedPath()
	for _, route := range snapshot.routes {
		if requestPath == route.prefix || strings.HasPrefix(requestPath, route.prefix+"/") {
			route.handler.ServeHTTP(w, request)
			return
		}
	}
	http.NotFound(w, request)
}

func (r *Registry) buildSnapshot(services []Service) (*registrySnapshot, error) {
	if len(services) > MaximumServices {
		return nil, fmt.Errorf("serve: service count %d exceeds %d", len(services), MaximumServices)
	}
	seen := make(map[string]struct{}, len(services))
	listeners := make(map[uint16]string)
	snapshot := &registrySnapshot{
		services: make([]Service, 0, len(services)),
		routes:   make([]serviceRoute, 0, len(services)),
	}
	for _, service := range services {
		normalized, err := normalizeService(service)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[normalized.Name]; exists {
			return nil, fmt.Errorf("serve: duplicate service route %q", normalized.Name)
		}
		seen[normalized.Name] = struct{}{}
		for _, listen := range normalized.Listens {
			if owner, taken := listeners[listen.Public]; taken {
				return nil, fmt.Errorf("serve: routes %s and %s both listen on port %d", owner, normalized.Route(), listen.Public)
			}
			listeners[listen.Public] = normalized.Route()
		}
		snapshot.services = append(snapshot.services, normalized)
		if normalized.LocalOnly {
			continue
		}
		prefix := "/" + normalized.Name
		if prefixesOverlap(prefix, r.reservedPrefix) {
			return nil, fmt.Errorf("serve: service route %q overlaps reserved prefix %s", normalized.Name, r.reservedPrefix)
		}
		routed := normalized
		if routed.Kind == Proxy {
			routed.Target = routed.UpstreamPort()
		}
		handler, err := handlerForNormalizedService(routed, prefix, r.trustForwardedHeaders)
		if err != nil {
			return nil, err
		}
		if normalized.Demand != nil {
			handler = r.gatedHandler(normalized.Name, handler)
		}
		snapshot.routes = append(snapshot.routes, serviceRoute{prefix: prefix, handler: handler})
	}
	sort.Slice(snapshot.services, func(i, j int) bool {
		return snapshot.services[i].Name < snapshot.services[j].Name
	})
	sort.Slice(snapshot.routes, func(i, j int) bool {
		if len(snapshot.routes[i].prefix) != len(snapshot.routes[j].prefix) {
			return len(snapshot.routes[i].prefix) > len(snapshot.routes[j].prefix)
		}
		return snapshot.routes[i].prefix < snapshot.routes[j].prefix
	})
	return snapshot, nil
}

func normalizeService(service Service) (Service, error) {
	if err := validateRouteName(service.Name); err != nil {
		return Service{}, err
	}
	if err := validatePublicName(service.PublicName); err != nil {
		return Service{}, fmt.Errorf("serve: service %q: %w", service.Name, err)
	}
	if len(service.Target) > MaximumServiceTargetBytes {
		return Service{}, fmt.Errorf("serve: service %q target exceeds %d bytes", service.Name, MaximumServiceTargetBytes)
	}
	switch service.Kind {
	case Static, Files:
		if service.Target == "" {
			return Service{}, fmt.Errorf("serve: service %q has an empty directory target", service.Name)
		}
		target, err := filepath.Abs(service.Target)
		if err != nil {
			return Service{}, fmt.Errorf("serve: resolve service %q target %s: %w", service.Name, service.Target, err)
		}
		service.Target = filepath.Clean(target)
		if len(service.Target) > MaximumServiceTargetBytes {
			return Service{}, fmt.Errorf("serve: service %q resolved target exceeds %d bytes", service.Name, MaximumServiceTargetBytes)
		}
	case Proxy:
		port, err := strconv.ParseUint(service.Target, 10, 16)
		if err != nil || port == 0 {
			return Service{}, fmt.Errorf("serve: service %q proxy target %q is not a port from 1 to 65535", service.Name, service.Target)
		}
		service.Target = strconv.FormatUint(port, 10)
	default:
		return Service{}, fmt.Errorf("serve: service %q has unsupported kind %q", service.Name, service.Kind)
	}
	return normalizeDemand(service)
}

func validatePublicName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > 253 || name != strings.ToLower(name) || strings.HasSuffix(name, ".") {
		return fmt.Errorf("public name %q is not a canonical hostname", name)
	}
	suffix := "." + PublicDomain
	if !strings.HasSuffix(name, suffix) {
		return fmt.Errorf("public name %q is not one label below %s", name, PublicDomain)
	}
	label := strings.TrimSuffix(name, suffix)
	if label == "" || strings.Contains(label, ".") {
		return fmt.Errorf("public name %q is not one label below %s", name, PublicDomain)
	}
	if label == "mesh" {
		return fmt.Errorf("public name %q is reserved for private naming", name)
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("public name %q has an invalid DNS label", name)
		}
		for _, character := range label {
			letter := character >= 'a' && character <= 'z'
			digit := character >= '0' && character <= '9'
			if !letter && !digit && character != '-' {
				return fmt.Errorf("public name %q has an invalid DNS label", name)
			}
		}
	}
	return nil
}

func validateRouteName(name string) error {
	if len(name) > MaximumServiceNameBytes {
		return fmt.Errorf("serve: service route exceeds %d bytes", MaximumServiceNameBytes)
	}
	if name == "" || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("serve: service route %q must be a clean relative path", name)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("serve: service route %q contains an invalid segment", name)
		}
		for _, character := range segment {
			if !routeCharacter(character) {
				return fmt.Errorf("serve: service route %q contains unsupported characters", name)
			}
		}
	}
	return nil
}

func prefixesOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func routeCharacter(character rune) bool {
	return character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' ||
		strings.ContainsRune("-._~", character)
}
