package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

type serviceStore interface {
	ListServices(context.Context) ([]meshserve.Service, error)
	UpsertService(context.Context, meshserve.Service) (meshserve.Service, error)
	DeleteService(context.Context, string) error
}

type servicePublisher interface {
	Enabled() bool
	Converge(context.Context, []meshserve.Service) error
	ListPage(context.Context, string, int) ([]protocol.EdgeRouteInfo, string, error)
}

type disabledServicePublisher struct{}

func (disabledServicePublisher) Enabled() bool { return false }
func (disabledServicePublisher) Converge(context.Context, []meshserve.Service) error {
	return nil
}
func (disabledServicePublisher) ListPage(context.Context, string, int) ([]protocol.EdgeRouteInfo, string, error) {
	return nil, "", errors.New("daemon: public edge is not configured")
}

const (
	publicServiceHeartbeatInterval = time.Minute
	publicServiceRollbackTimeout   = 20 * time.Second
)

// serviceController serializes SQLite writes with publication of a complete
// live routing snapshot. SQLite commits first, so a crash between the two steps
// converges when the next daemon restores the registry.
type serviceController struct {
	lifetime       context.Context
	home           string
	store          serviceStore
	registry       *meshserve.Registry
	publisher      servicePublisher
	gate           chan struct{}
	unsynced       bool
	catalogUnknown bool
}

func newServiceController(ctx context.Context, home string, store serviceStore, registry *meshserve.Registry, publisher servicePublisher) (*serviceController, error) {
	if ctx == nil {
		return nil, errors.New("daemon: nil service controller context")
	}
	if store == nil {
		return nil, errors.New("daemon: nil service store")
	}
	if registry == nil {
		return nil, errors.New("daemon: nil service registry")
	}
	if publisher == nil {
		return nil, errors.New("daemon: nil public service publisher")
	}
	if !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return nil, errors.New("daemon: service home must be a clean absolute path")
	}
	if !publisher.Enabled() {
		for _, service := range registry.Services() {
			if service.PublicName != "" {
				return nil, errors.New("daemon: persisted public service requires a configured public edge")
			}
		}
	}
	return &serviceController{lifetime: ctx, home: home, store: store, registry: registry, publisher: publisher, gate: make(chan struct{}, 1)}, nil
}

func (c *serviceController) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	switch request.Type {
	case protocol.TypeServicePreview:
		if ctx == nil {
			return protocol.Control{}, true, fmt.Errorf("daemon: %s request has nil context", request.Type)
		}
		response, err := c.preview(ctx, request)
		return response, true, err
	case protocol.TypeServiceUpsert:
		if ctx == nil {
			return protocol.Control{}, true, fmt.Errorf("daemon: %s request has nil context", request.Type)
		}
		response, err := c.upsert(ctx, request)
		return response, true, err
	case protocol.TypeServiceList:
		if ctx == nil {
			return protocol.Control{}, true, fmt.Errorf("daemon: %s request has nil context", request.Type)
		}
		response, err := c.list(ctx, request)
		return response, true, err
	case protocol.TypeServiceDelete:
		if ctx == nil {
			return protocol.Control{}, true, fmt.Errorf("daemon: %s request has nil context", request.Type)
		}
		response, err := c.delete(ctx, request)
		return response, true, err
	case protocol.TypeEdgeList:
		if !c.publisher.Enabled() {
			return protocol.Control{}, false, nil
		}
		if ctx == nil {
			return protocol.Control{}, true, fmt.Errorf("daemon: %s request has nil context", request.Type)
		}
		response, err := c.edgeList(ctx, request)
		return response, true, err
	default:
		return protocol.Control{}, false, nil
	}
}

func (c *serviceController) preview(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	if request.Service == nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s request has no service", request.Type)
	}
	preview, err := meshserve.InspectService(ctx, c.home, serviceFromInfo(*request.Service), request.AllowCredentials)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, err)
	}
	return protocol.Control{
		Type: protocol.TypeServicePreviewed, RequestID: request.RequestID,
		ServicePreview: &protocol.ServicePreview{
			Service: serviceDefinitionInfo(preview.Service), FileCount: preview.FileCount,
		},
	}, nil
}

func (c *serviceController) upsert(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	if request.Service == nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s request has no service", request.Type)
	}
	if err := ctx.Err(); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s request: %w", request.Type, err)
	}

	if err := c.acquire(ctx); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s request: %w", request.Type, err)
	}
	defer c.release()
	if err := c.ensureSynchronized("service upsert"); err != nil {
		return protocol.Control{}, err
	}
	preview, err := meshserve.InspectService(ctx, c.home, serviceFromInfo(*request.Service), request.AllowCredentials)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, err)
	}
	if request.ServicePreview != nil && !sameServicePreview(preview, *request.ServicePreview) {
		return protocol.Control{}, errors.New("daemon: service changed after preview; preview it again")
	}
	service := preview.Service
	priorServices := c.registry.Services()
	prior, hadPrior := findService(priorServices, service.Name)
	if hadPrior && prior.PublicName != "" && prior.PublicName != service.PublicName {
		return protocol.Control{}, errors.New("daemon: changing or removing a public name requires unserve first")
	}
	publicMutation := service.PublicName != "" || hadPrior && prior.PublicName != ""
	if publicMutation && !c.publisher.Enabled() {
		return protocol.Control{}, errors.New("daemon: public service requires a configured public edge")
	}
	services := upsertService(priorServices, service)
	if _, err := meshserve.NewRegistryWithReservedPrefix(services, c.registry.ReservedPrefix(), nil); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, err)
	}
	persisted, err := c.store.UpsertService(ctx, service)
	if err != nil {
		reconcileErr := c.reconcileDurable("upsert error")
		return protocol.Control{}, errors.Join(fmt.Errorf("daemon: %s: %w", request.Type, err), reconcileErr)
	}
	services = upsertService(services, persisted)
	if err := c.registry.Replace(services); err != nil {
		reconcileErr := c.reconcileDurable("service registry publication")
		return protocol.Control{}, errors.Join(fmt.Errorf("daemon: publish service %s: %w", persisted.Name, err), reconcileErr)
	}
	if publicMutation {
		if publishErr := c.publisher.Converge(ctx, services); publishErr != nil {
			rollbackErr := c.rollbackUpsert(priorServices, prior, hadPrior, persisted.Name)
			return protocol.Control{}, errors.Join(fmt.Errorf("daemon: public edge did not acknowledge service %s: %w", persisted.Name, publishErr), rollbackErr)
		}
	}
	info := serviceDefinitionInfo(persisted)
	for _, status := range c.registry.Status() {
		if status.Service.Name == persisted.Name {
			info.Healthy = status.Healthy
			info.Problem = boundedServiceProblem(status.Problem)
			break
		}
	}
	return protocol.Control{
		Type:      protocol.TypeServiceUpserted,
		RequestID: request.RequestID,
		Service:   &info,
	}, nil
}

func (c *serviceController) list(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	if err := ctx.Err(); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s request: %w", request.Type, err)
	}
	if err := c.acquire(ctx); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s request: %w", request.Type, err)
	}
	defer c.release()
	if c.catalogUnknown {
		reconcileErr := c.reconcileDurable("service list")
		if c.catalogUnknown {
			return protocol.Control{}, fmt.Errorf("daemon: service catalog is unavailable: %w", reconcileErr)
		}
	}
	statuses := c.registry.Status()
	services := make([]protocol.ServiceInfo, 0, len(statuses))
	for _, status := range statuses {
		info := serviceDefinitionInfo(status.Service)
		info.Healthy = status.Healthy
		info.Problem = boundedServiceProblem(status.Problem)
		services = append(services, info)
	}
	return protocol.Control{
		Type:      protocol.TypeServiceListed,
		RequestID: request.RequestID,
		Services:  services,
	}, nil
}

func (c *serviceController) delete(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	if err := meshserve.ValidateName(request.ServiceName); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, err)
	}
	if err := ctx.Err(); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s request: %w", request.Type, err)
	}

	if err := c.acquire(ctx); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s request: %w", request.Type, err)
	}
	defer c.release()
	if err := c.ensureSynchronized("service deletion"); err != nil {
		return protocol.Control{}, err
	}
	if err := c.store.DeleteService(ctx, request.ServiceName); err != nil && !errors.Is(err, sql.ErrNoRows) {
		reconcileErr := c.reconcileDurable("delete error")
		return protocol.Control{}, errors.Join(fmt.Errorf("daemon: %s: %w", request.Type, err), reconcileErr)
	}
	services := deleteService(c.registry.Services(), request.ServiceName)
	if err := c.registry.Replace(services); err != nil {
		reconcileErr := c.reconcileDurable("service deletion registry publication")
		return protocol.Control{}, errors.Join(fmt.Errorf("daemon: unpublish service %s: %w", request.ServiceName, err), reconcileErr)
	}
	if c.publisher.Enabled() {
		if err := c.publisher.Converge(ctx, services); err != nil {
			c.unsynced = true
			return protocol.Control{}, fmt.Errorf("daemon: public edge did not acknowledge service deletion: %w", err)
		}
	}
	return protocol.Control{
		Type:        protocol.TypeServiceDeleted,
		RequestID:   request.RequestID,
		ServiceName: request.ServiceName,
	}, nil
}

func (c *serviceController) edgeList(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	routes, next, err := c.publisher.ListPage(ctx, request.EdgeCursor, request.EdgeLimit)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: list public edge routes: %w", err)
	}
	return protocol.Control{
		Type: protocol.TypeEdgeListed, RequestID: request.RequestID,
		EdgeRoutes: routes, EdgeNextCursor: next,
	}, nil
}

// SyncPublic derives and publishes desired state while holding the same gate
// as mutations. A heartbeat can therefore never overtake a newer upsert or
// deletion with a stale service snapshot.
func (c *serviceController) SyncPublic(ctx context.Context) error {
	if !c.publisher.Enabled() {
		return nil
	}
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	if c.unsynced {
		return c.reconcileDurable("pending public synchronization")
	}
	if err := c.publisher.Converge(ctx, c.registry.Services()); err != nil {
		c.unsynced = true
		return err
	}
	return nil
}

func (c *serviceController) RunPublicHeartbeat(ctx context.Context, report func(error)) {
	if ctx == nil || !c.publisher.Enabled() {
		return
	}
	if report == nil {
		report = func(error) {}
	}
	syncNow := func() {
		if err := c.SyncPublic(ctx); err != nil && ctx.Err() == nil {
			report(err)
		}
	}
	syncNow()
	ticker := time.NewTicker(publicServiceHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			syncNow()
		}
	}
}

func (c *serviceController) rollbackUpsert(priorServices []meshserve.Service, prior meshserve.Service, hadPrior bool, changedName string) error {
	c.unsynced = true
	rollbackContext, cancel := context.WithTimeout(c.lifetime, publicServiceRollbackTimeout)
	defer cancel()
	var rollbackErr error
	if hadPrior {
		_, rollbackErr = c.store.UpsertService(rollbackContext, prior)
	} else {
		rollbackErr = c.store.DeleteService(rollbackContext, changedName)
		if errors.Is(rollbackErr, sql.ErrNoRows) {
			rollbackErr = nil
		}
	}
	if rollbackErr != nil {
		return errors.Join(fmt.Errorf("daemon: restore prior durable service: %w", rollbackErr), c.reconcileDurable("ambiguous rollback"))
	}
	if err := c.registry.Replace(priorServices); err != nil {
		return errors.Join(rollbackErr, fmt.Errorf("daemon: restore prior service registry: %w", err), c.reconcileDurable("rollback registry publication"))
	}
	if c.publisher.Enabled() {
		if err := c.publisher.Converge(rollbackContext, priorServices); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("daemon: publish public service rollback: %w", err))
		} else {
			c.unsynced = false
		}
	} else {
		c.unsynced = false
	}
	return rollbackErr
}

// reconcileDurable reloads the only authoritative service state after a
// potentially post-commit SQLite error. The caller holds c.gate.
func (c *serviceController) reconcileDurable(operation string) error {
	c.unsynced = true
	ctx, cancel := context.WithTimeout(c.lifetime, publicServiceRollbackTimeout)
	defer cancel()
	services, err := c.store.ListServices(ctx)
	if err != nil {
		c.catalogUnknown = true
		clearErr := c.registry.Replace(nil)
		return errors.Join(fmt.Errorf("daemon: reload services after %s: %w", operation, err), clearErr)
	}
	if err := c.registry.Replace(services); err != nil {
		c.catalogUnknown = true
		clearErr := c.registry.Replace(nil)
		return errors.Join(fmt.Errorf("daemon: publish durable services after %s: %w", operation, err), clearErr)
	}
	c.catalogUnknown = false
	if c.publisher.Enabled() {
		if err := c.publisher.Converge(ctx, services); err != nil {
			return fmt.Errorf("daemon: publish durable public services after %s: %w", operation, err)
		}
	}
	c.unsynced = false
	return nil
}

func (c *serviceController) ensureSynchronized(operation string) error {
	if !c.unsynced {
		return nil
	}
	if err := c.reconcileDurable(operation); err != nil {
		return fmt.Errorf("daemon: service state is not synchronized: %w", err)
	}
	return nil
}

func (c *serviceController) acquire(ctx context.Context) error {
	select {
	case c.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *serviceController) release() { <-c.gate }

func findService(services []meshserve.Service, name string) (meshserve.Service, bool) {
	for _, service := range services {
		if service.Name == name {
			return service, true
		}
	}
	return meshserve.Service{Name: name}, false
}

func upsertService(services []meshserve.Service, service meshserve.Service) []meshserve.Service {
	updated := append([]meshserve.Service(nil), services...)
	for index := range updated {
		if updated[index].Name == service.Name {
			updated[index] = service
			return updated
		}
	}
	return append(updated, service)
}

func deleteService(services []meshserve.Service, name string) []meshserve.Service {
	updated := make([]meshserve.Service, 0, len(services))
	for _, service := range services {
		if service.Name != name {
			updated = append(updated, service)
		}
	}
	return updated
}

func serviceFromInfo(info protocol.ServiceInfo) meshserve.Service {
	return meshserve.Service{
		Name:          info.Name,
		Kind:          meshserve.Kind(info.Kind),
		Target:        info.Target,
		PublicName:    info.PublicName,
		WakeOnRequest: info.WakeOnRequest,
		Isolate:       info.Isolate,
	}
}

func serviceDefinitionInfo(service meshserve.Service) protocol.ServiceInfo {
	return protocol.ServiceInfo{
		Name:          service.Name,
		Kind:          string(service.Kind),
		Target:        service.Target,
		PublicName:    service.PublicName,
		WakeOnRequest: service.WakeOnRequest,
		Isolate:       service.Isolate,
	}
}

func sameServicePreview(actual meshserve.Preview, expected protocol.ServicePreview) bool {
	return actual.Service == serviceFromInfo(expected.Service) && actual.FileCount == expected.FileCount
}

func boundedServiceProblem(problem string) string {
	problem = strings.ToValidUTF8(problem, "?")
	if len(problem) <= meshserve.MaximumServiceProblemBytes {
		return problem
	}
	limit := meshserve.MaximumServiceProblemBytes - len("…")
	for limit > 0 && !utf8.ValidString(problem[:limit]) {
		limit--
	}
	return problem[:limit] + "…"
}
