package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shaul/mesh/internal/dnsname"
	meshserve "github.com/shaul/mesh/internal/serve"
	dbsqlc "github.com/shaul/mesh/internal/storage/sqlc"
)

const MaximumCachedServices = 8192

// ReplaceCachedServices atomically records host and replaces its complete
// cached service list after one successful live query.
func (s *Store) ReplaceCachedServices(ctx context.Context, host Host, services []CachedService) error {
	if ctx == nil {
		return errors.New("storage: nil cached-service context")
	}
	if len(services) > meshserve.MaximumServices {
		return fmt.Errorf("storage: cached service count %d exceeds %d", len(services), meshserve.MaximumServices)
	}
	hostValues, err := hostParams(host)
	if err != nil {
		return err
	}
	rows := make([]dbsqlc.UpsertCachedServiceParams, len(services))
	seen := make(map[string]struct{}, len(services))
	for index, service := range services {
		row, err := cachedServiceParams(service)
		if err != nil {
			return err
		}
		if service.HostID != host.ID {
			return fmt.Errorf("storage: cached service %s belongs to host %s, want %s", service.Service.Name, service.HostID, host.ID)
		}
		if _, exists := seen[service.Service.Name]; exists {
			return fmt.Errorf("storage: duplicate cached service %s", service.Service.Name)
		}
		seen[service.Service.Name] = struct{}{}
		rows[index] = row
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin cached-service replacement: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // commit is authoritative
	queries := s.queries.WithTx(tx)
	if _, err := queries.UpsertHost(ctx, hostValues); err != nil {
		return fmt.Errorf("storage: cache service host %s: %w", host.ID, err)
	}
	if err := queries.DeleteCachedServicesForHost(ctx, string(host.ID)); err != nil {
		return fmt.Errorf("storage: clear cached services for host %s: %w", host.ID, err)
	}
	for _, row := range rows {
		if err := queries.UpsertCachedService(ctx, row); err != nil {
			return fmt.Errorf("storage: cache service %s/%s: %w", row.HostID, row.Name, err)
		}
	}
	total, err := queries.CountCachedServices(ctx)
	if err != nil {
		return fmt.Errorf("storage: count cached services: %w", err)
	}
	if total > MaximumCachedServices {
		return fmt.Errorf("storage: cached service count %d exceeds %d", total, MaximumCachedServices)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit cached services for host %s: %w", host.ID, err)
	}
	return nil
}

// ListCachedServices returns the last complete service list observed for host.
func (s *Store) ListCachedServices(ctx context.Context, hostID HostID) ([]CachedService, error) {
	if ctx == nil {
		return nil, errors.New("storage: nil cached-service context")
	}
	if err := validateHostID(hostID); err != nil {
		return nil, err
	}
	rows, err := s.queries.ListCachedServicesForHost(ctx, string(hostID))
	if err != nil {
		return nil, fmt.Errorf("storage: list cached services for host %s: %w", hostID, err)
	}
	if len(rows) > meshserve.MaximumServices {
		return nil, fmt.Errorf("storage: cached service count %d exceeds %d", len(rows), meshserve.MaximumServices)
	}
	result := make([]CachedService, len(rows))
	for index, row := range rows {
		cached, err := cachedServiceFromRow(row)
		if err != nil {
			return nil, fmt.Errorf("storage: decode cached service %s/%s: %w", row.HostID, row.Name, err)
		}
		result[index] = cached
	}
	return result, nil
}

// ListAllCachedServices returns a bounded snapshot for concurrent CLI fallback.
func (s *Store) ListAllCachedServices(ctx context.Context) ([]CachedService, error) {
	if ctx == nil {
		return nil, errors.New("storage: nil cached-service context")
	}
	rows, err := s.queries.ListCachedServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage: list cached services: %w", err)
	}
	if len(rows) > MaximumCachedServices {
		return nil, fmt.Errorf("storage: cached service count exceeds %d", MaximumCachedServices)
	}
	result := make([]CachedService, len(rows))
	for index, row := range rows {
		cached, err := cachedServiceFromRow(row)
		if err != nil {
			return nil, fmt.Errorf("storage: decode cached service %s/%s: %w", row.HostID, row.Name, err)
		}
		result[index] = cached
	}
	return result, nil
}

func cachedServiceParams(cached CachedService) (dbsqlc.UpsertCachedServiceParams, error) {
	if err := validateHostID(cached.HostID); err != nil {
		return dbsqlc.UpsertCachedServiceParams{}, err
	}
	if cached.PrivateName != "" {
		if err := dnsname.ValidatePrivateName(cached.PrivateName); err != nil {
			return dbsqlc.UpsertCachedServiceParams{}, fmt.Errorf("storage: cached private name: %w", err)
		}
	}
	service, err := meshserve.Normalize(cached.Service)
	if err != nil {
		return dbsqlc.UpsertCachedServiceParams{}, fmt.Errorf("storage: cached service on host %s: %w", cached.HostID, err)
	}
	if len(cached.Problem) > meshserve.MaximumServiceProblemBytes {
		return dbsqlc.UpsertCachedServiceParams{}, fmt.Errorf("storage: cached service %s problem exceeds %d bytes", service.Name, meshserve.MaximumServiceProblemBytes)
	}
	if cached.Healthy && cached.Problem != "" {
		return dbsqlc.UpsertCachedServiceParams{}, fmt.Errorf("storage: healthy cached service %s has a problem", service.Name)
	}
	observedAt, err := timeMillis("cached service observation", cached.ObservedAt)
	if err != nil {
		return dbsqlc.UpsertCachedServiceParams{}, err
	}
	return dbsqlc.UpsertCachedServiceParams{
		HostID: string(cached.HostID), PrivateName: cached.PrivateName, Name: service.Name, Kind: string(service.Kind), Target: service.Target,
		PublicName: service.PublicName, WakeOnRequest: boolInt64(service.WakeOnRequest), Healthy: boolInt64(cached.Healthy),
		Problem: cached.Problem, ObservedAt: observedAt,
	}, nil
}

func cachedServiceFromRow(row dbsqlc.CachedService) (CachedService, error) {
	wake, err := sqliteBool("wake_on_request", row.WakeOnRequest)
	if err != nil {
		return CachedService{}, err
	}
	healthy, err := sqliteBool("healthy", row.Healthy)
	if err != nil {
		return CachedService{}, err
	}
	cached := CachedService{
		HostID: HostID(row.HostID), PrivateName: row.PrivateName,
		Service: meshserve.Service{
			Name: row.Name, Kind: meshserve.Kind(row.Kind), Target: row.Target,
			PublicName: row.PublicName, WakeOnRequest: wake,
		},
		Healthy: healthy, Problem: row.Problem, ObservedAt: time.UnixMilli(row.ObservedAt).UTC(),
	}
	if _, err := cachedServiceParams(cached); err != nil {
		return CachedService{}, err
	}
	return cached, nil
}
