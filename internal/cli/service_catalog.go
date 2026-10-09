package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
)

const (
	serviceHealthUnknown            = "unknown"
	maximumConcurrentServiceQueries = 16

	maximumConcurrentCacheWrites = 4
	serviceCacheWriteTimeout     = 200 * time.Millisecond
)

type serviceQuery func(context.Context, HostRecord) (remoteServiceSnapshot, error)

type serviceCatalogCache interface {
	LoadAllServices(context.Context) (map[string][]storage.CachedService, error)
	SaveServices(context.Context, HostRecord, string, []protocol.ServiceInfo) error
}

// ServiceCatalogRow is one live or cached private service definition.
type ServiceCatalogRow struct {
	Host        HostRecord
	PrivateName string
	Service     protocol.ServiceInfo
	Live        bool
	Stale       bool
	ObservedAt  time.Time

	HealthUnsupported bool
}

func (r ServiceCatalogRow) Scope() string {
	if r.Service.LocalOnly {
		// Reached only through its listeners on the host itself.
		return "local"
	}

	return "tailnet"
}

func (r ServiceCatalogRow) URL() string {
	return serviceURL(r.Host, r.PrivateName, r.Service)
}

func (r ServiceCatalogRow) Health() string {
	if !r.Live {
		return "offline/stale"
	}
	if r.HealthUnsupported || r.Service.HealthUnknown {
		return serviceHealthUnknown
	}
	if state, ok := r.demandHealth(); ok {
		return state
	}
	if !r.Service.Healthy {
		return "unhealthy"
	}

	return "healthy"
}

type serviceQueryResult struct {
	host     HostRecord
	snapshot remoteServiceSnapshot
	err      error
}

type serviceCacheResult struct {
	host HostRecord
	err  error
}

type catalogCacheWarning struct{ err error }

func (w catalogCacheWarning) Error() string { return "cache live services: " + w.err.Error() }
func (w catalogCacheWarning) Unwrap() error { return w.err }

// CollectServiceCatalog fans out one-shot live queries under one deadline and
// falls back to the complete cached list for each unavailable host.
func CollectServiceCatalog(ctx context.Context, hosts []HostRecord, timeout time.Duration, query serviceQuery, cache serviceCatalogCache) ([]ServiceCatalogRow, map[string]error, error) {
	if ctx == nil {
		return nil, nil, errors.New("cli: nil service catalog context")
	}
	if timeout <= 0 {
		return nil, nil, errors.New("cli: service catalog timeout must be positive")
	}
	if query == nil || cache == nil {
		return nil, nil, errors.New("cli: incomplete service catalog dependencies")
	}
	if len(hosts) > maximumConfiguredHosts {
		return nil, nil, fmt.Errorf("cli: host count %d exceeds %d", len(hosts), maximumConfiguredHosts)
	}

	operationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cached, err := cache.LoadAllServices(operationCtx)
	if err != nil {
		return nil, nil, fmt.Errorf("load cached services: %w", err)
	}
	observedHosts := make(map[string]HostRecord, len(hosts))
	rowsByHost := make(map[string][]ServiceCatalogRow, len(hosts))
	rowCount := 0
	for _, host := range hosts {
		observedHosts[host.ID] = host
		rowsByHost[host.ID] = cachedServiceCatalogRows(host, cached[host.ID])
		rowCount += len(rowsByHost[host.ID])
	}

	serviceResults := make(chan serviceQueryResult, len(hosts))

	cacheResults := make(chan serviceCacheResult, len(hosts))
	serviceSemaphore := make(chan struct{}, maximumConcurrentServiceQueries)

	cacheSemaphore := make(chan struct{}, maximumConcurrentCacheWrites)
	for _, host := range hosts {
		host := host
		go func() {
			if !acquireQuery(operationCtx, serviceSemaphore) {
				return
			}
			snapshot, queryErr := query(operationCtx, host)
			<-serviceSemaphore
			select {
			case serviceResults <- serviceQueryResult{host: host, snapshot: snapshot, err: queryErr}:
			case <-operationCtx.Done():
			}
		}()

	}

	diagnostics := make(map[string]error)

	completedServices := make(map[string]bool, len(hosts))
	wantServices := len(hosts)

	wantCacheWrites := 0

	for wantServices > 0 || wantCacheWrites > 0 {
		select {
		case result := <-serviceResults:
			wantServices--
			completedServices[result.host.ID] = true
			if result.err != nil {
				diagnostics[result.host.ID] = result.err
				continue
			}
			newCount := rowCount - len(rowsByHost[result.host.ID]) + len(result.snapshot.Services)
			if newCount > storage.MaximumCachedServices {
				return nil, nil, fmt.Errorf("service catalog exceeds %d rows", storage.MaximumCachedServices)
			}
			if result.snapshot.Host.ID != "" {
				result.host = result.snapshot.Host
			}
			observedHosts[result.host.ID] = result.host
			live := liveServiceCatalogRows(result.host, result.snapshot)
			rowsByHost[result.host.ID] = live
			rowCount = newCount
			wantCacheWrites++
			go func(host HostRecord, privateName string, services []protocol.ServiceInfo) {
				if !acquireQuery(operationCtx, cacheSemaphore) {
					return
				}
				cacheCtx, cacheCancel := context.WithTimeout(operationCtx, serviceCacheWriteTimeout)
				cacheErr := cache.SaveServices(cacheCtx, host, privateName, services)
				cacheCancel()
				<-cacheSemaphore
				select {
				case cacheResults <- serviceCacheResult{host: host, err: cacheErr}:
				case <-operationCtx.Done():
				}
			}(result.host, result.snapshot.PrivateName, append([]protocol.ServiceInfo(nil), result.snapshot.Services...))
		case result := <-cacheResults:
			wantCacheWrites--
			if result.err != nil && operationCtx.Err() == nil {
				diagnostics[result.host.ID] = catalogCacheWarning{err: result.err}
			}
		case <-operationCtx.Done():
			wantServices = 0

			wantCacheWrites = 0
		}
	}
	cancel()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	rows := make([]ServiceCatalogRow, 0)
	for _, host := range hosts {
		for _, row := range rowsByHost[host.ID] {

			rows = append(rows, row)
			if len(rows) > storage.MaximumCachedServices {
				return nil, nil, fmt.Errorf("service catalog exceeds %d rows", storage.MaximumCachedServices)
			}
		}
		if _, exists := diagnostics[host.ID]; !exists && !completedServices[host.ID] {
			diagnostics[host.ID] = context.DeadlineExceeded
		}
	}
	projected := make([]HostRecord, 0, len(hosts))
	for _, host := range hosts {
		current := observedHosts[host.ID]
		projected = append(projected, current)
	}
	ProjectHostNames(projected)
	byID := make(map[string]HostRecord, len(projected))
	for _, host := range projected {
		byID[host.ID] = host
	}
	for index := range rows {
		rows[index].Host = byID[rows[index].Host.ID]
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Host.MachineName != rows[j].Host.MachineName {
			return rows[i].Host.MachineName < rows[j].Host.MachineName
		}
		return rows[i].Service.Name < rows[j].Service.Name
	})
	return rows, diagnostics, nil
}

func cachedServiceCatalogRows(host HostRecord, cached []storage.CachedService) []ServiceCatalogRow {
	rows := make([]ServiceCatalogRow, len(cached))
	for index, row := range cached {
		rows[index] = ServiceCatalogRow{
			Host: host, PrivateName: row.PrivateName,
			Service: protocol.ServiceInfo{
				DisplayName: row.Service.DisplayName, Name: row.Service.Name, Kind: string(row.Service.Kind), Target: row.Service.Target,
				PrivateHost: row.Service.PrivateHost, Isolate: row.Service.Isolate, LocalOnly: row.Service.LocalOnly,
				Healthy: row.Healthy, Problem: row.Problem,
			},
			Stale: true, ObservedAt: row.ObservedAt,
		}
	}
	return rows
}

func liveServiceCatalogRows(host HostRecord, snapshot remoteServiceSnapshot) []ServiceCatalogRow {
	rows := make([]ServiceCatalogRow, len(snapshot.Services))
	for index, service := range snapshot.Services {
		rows[index] = ServiceCatalogRow{Host: host, PrivateName: snapshot.PrivateName, Service: service, Live: true, HealthUnsupported: !snapshot.ServiceHealthSupported}
	}
	return rows
}

func acquireQuery(ctx context.Context, semaphore chan struct{}) bool {
	select {
	case semaphore <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func catalogCandidates(rows []ServiceCatalogRow, route, hostID string) []ServiceCatalogRow {
	candidates := make([]ServiceCatalogRow, 0)
	for _, row := range rows {
		if row.Service.Name != route {
			continue
		}
		if hostID != "" && row.Host.ID != hostID {
			continue
		}
		candidates = append(candidates, row)
	}
	return candidates
}
