package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

type serviceStore interface {
	UpsertService(context.Context, meshserve.Service) (meshserve.Service, error)
	DeleteService(context.Context, string) error
}

// serviceController serializes SQLite writes with publication of a complete
// live routing snapshot. SQLite commits first, so a crash between the two steps
// converges when the next daemon restores the registry.
type serviceController struct {
	store    serviceStore
	registry *meshserve.Registry
	mu       sync.Mutex
}

func newServiceController(store serviceStore, registry *meshserve.Registry) (*serviceController, error) {
	if store == nil {
		return nil, errors.New("daemon: nil service store")
	}
	if registry == nil {
		return nil, errors.New("daemon: nil service registry")
	}
	return &serviceController{store: store, registry: registry}, nil
}

func (c *serviceController) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	switch request.Type {
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
	default:
		return protocol.Control{}, false, nil
	}
}

func (c *serviceController) upsert(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	if request.Service == nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s request has no service", request.Type)
	}
	service, err := meshserve.Normalize(serviceFromInfo(*request.Service))
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, err)
	}
	if err := ctx.Err(); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s request: %w", request.Type, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	services := upsertService(c.registry.Services(), service)
	if _, err := meshserve.NewRegistryWithReservedPrefix(services, c.registry.ReservedPrefix()); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, err)
	}
	persisted, err := c.store.UpsertService(ctx, service)
	if err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, err)
	}
	services = upsertService(services, persisted)
	if err := c.registry.Replace(services); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: publish service %s: %w", persisted.Name, err)
	}
	info := serviceDefinitionInfo(persisted)
	for _, status := range c.registry.Status() {
		if status.Service.Name == persisted.Name {
			info.Healthy = status.Healthy
			info.Problem = status.Problem
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
	statuses := c.registry.Status()
	services := make([]protocol.ServiceInfo, 0, len(statuses))
	for _, status := range statuses {
		info := serviceDefinitionInfo(status.Service)
		info.Healthy = status.Healthy
		info.Problem = status.Problem
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

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.store.DeleteService(ctx, request.ServiceName); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return protocol.Control{}, fmt.Errorf("daemon: %s: %w", request.Type, err)
	}
	services := deleteService(c.registry.Services(), request.ServiceName)
	if err := c.registry.Replace(services); err != nil {
		return protocol.Control{}, fmt.Errorf("daemon: unpublish service %s: %w", request.ServiceName, err)
	}
	return protocol.Control{
		Type:        protocol.TypeServiceDeleted,
		RequestID:   request.RequestID,
		ServiceName: request.ServiceName,
	}, nil
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
	}
}

func serviceDefinitionInfo(service meshserve.Service) protocol.ServiceInfo {
	return protocol.ServiceInfo{
		Name:          service.Name,
		Kind:          string(service.Kind),
		Target:        service.Target,
		PublicName:    service.PublicName,
		WakeOnRequest: service.WakeOnRequest,
	}
}
