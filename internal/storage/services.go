package storage

import (
	"context"
	"database/sql"
	"fmt"

	meshserve "github.com/shaul/mesh/internal/serve"
	dbsqlc "github.com/shaul/mesh/internal/storage/sqlc"
)

// UpsertService records the complete definition of one origin service.
func (s *Store) UpsertService(ctx context.Context, service meshserve.Service) (meshserve.Service, error) {
	normalized, err := meshserve.Normalize(service)
	if err != nil {
		return meshserve.Service{}, err
	}
	row, err := s.queries.UpsertService(ctx, dbsqlc.UpsertServiceParams{
		Name:          normalized.Name,
		Kind:          string(normalized.Kind),
		Target:        normalized.Target,
		PublicName:    normalized.PublicName,
		WakeOnRequest: boolInt64(normalized.WakeOnRequest),
	})
	if err != nil {
		return meshserve.Service{}, fmt.Errorf("storage: upsert service %s: %w", normalized.Name, err)
	}
	return serviceFromRow(row)
}

// GetService returns one service by its route name.
func (s *Store) GetService(ctx context.Context, name string) (meshserve.Service, error) {
	if err := meshserve.ValidateName(name); err != nil {
		return meshserve.Service{}, err
	}
	row, err := s.queries.GetService(ctx, name)
	if err != nil {
		return meshserve.Service{}, fmt.Errorf("storage: get service %s: %w", name, err)
	}
	return serviceFromRow(row)
}

// ListServices returns every service in route-name order.
func (s *Store) ListServices(ctx context.Context) ([]meshserve.Service, error) {
	rows, err := s.queries.ListServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage: list services: %w", err)
	}
	services := make([]meshserve.Service, 0, len(rows))
	for _, row := range rows {
		service, err := serviceFromRow(row)
		if err != nil {
			return nil, fmt.Errorf("storage: list services: %w", err)
		}
		services = append(services, service)
	}
	return services, nil
}

// DeleteService removes one service by its route name.
func (s *Store) DeleteService(ctx context.Context, name string) error {
	if err := meshserve.ValidateName(name); err != nil {
		return err
	}
	deleted, err := s.queries.DeleteService(ctx, name)
	if err != nil {
		return fmt.Errorf("storage: delete service %s: %w", name, err)
	}
	if deleted == 0 {
		return fmt.Errorf("storage: delete service %s: %w", name, sql.ErrNoRows)
	}
	return nil
}

func serviceFromRow(row dbsqlc.Service) (meshserve.Service, error) {
	wakeOnRequest, err := sqliteBool("wake_on_request", row.WakeOnRequest)
	if err != nil {
		return meshserve.Service{}, fmt.Errorf("storage: decode service %s: %w", row.Name, err)
	}
	service, err := meshserve.Normalize(meshserve.Service{
		Name:          row.Name,
		Kind:          meshserve.Kind(row.Kind),
		Target:        row.Target,
		PublicName:    row.PublicName,
		WakeOnRequest: wakeOnRequest,
	})
	if err != nil {
		return meshserve.Service{}, fmt.Errorf("storage: decode service %s: %w", row.Name, err)
	}
	return service, nil
}

func boolInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func sqliteBool(name string, value int64) (bool, error) {
	switch value {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, fmt.Errorf("%s is %d, want 0 or 1", name, value)
	}
}
