// Package serve exposes origin-side HTTP services and path confinement.
package serve

import (
	"fmt"
	"net/http"
	"net/netip"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
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
	snapshot, err := buildRegistrySnapshot(services, r.reservedPrefix, r.trustForwardedHeaders)
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

// Status returns live health for every registered service.
func (r *Registry) Status() []ServiceStatus {
	services := r.Services()
	statuses := make([]ServiceStatus, 0, len(services))
	for _, service := range services {
		status := ServiceStatus{Service: service, Healthy: true}
		if service.Kind == Static || service.Kind == Files {
			file, _, err := OpenRootEntry(service.Target, "/")
			if err == nil {
				_ = file.Close()
			}
			if err != nil {
				status.Healthy = false
				status.Problem = err.Error()
			}
		}
		statuses = append(statuses, status)
	}
	return statuses
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

func buildRegistrySnapshot(services []Service, reservedPrefix string, trustForwardedHeaders func(netip.Addr) bool) (*registrySnapshot, error) {
	if len(services) > MaximumServices {
		return nil, fmt.Errorf("serve: service count %d exceeds %d", len(services), MaximumServices)
	}
	seen := make(map[string]struct{}, len(services))
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
		prefix := "/" + normalized.Name
		if prefixesOverlap(prefix, reservedPrefix) {
			return nil, fmt.Errorf("serve: service route %q overlaps reserved prefix %s", normalized.Name, reservedPrefix)
		}
		handler, err := handlerForNormalizedService(normalized, prefix, trustForwardedHeaders)
		if err != nil {
			return nil, err
		}
		snapshot.services = append(snapshot.services, normalized)
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
	return service, nil
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
