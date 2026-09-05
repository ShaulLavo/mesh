package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
)

const (
	maximumConcurrentServiceQueries = 16
	maximumConcurrentEdgeQueries    = 4
	maximumConcurrentCacheWrites    = 4
	serviceCacheWriteTimeout        = 200 * time.Millisecond
)

type serviceQuery func(context.Context, HostRecord) (remoteServiceSnapshot, error)
type edgeQuery func(context.Context, HostRecord) ([]protocol.EdgeRouteInfo, error)

type serviceCatalogCache interface {
	LoadAllServices(context.Context) (map[string][]storage.CachedService, error)
	SaveServices(context.Context, HostRecord, string, []protocol.ServiceInfo) error
}

// ServiceCatalogRow joins one live or cached origin definition to any public
// edge status that could be obtained within the same hard deadline.
type ServiceCatalogRow struct {
	Host        HostRecord
	PrivateName string
	Service     protocol.ServiceInfo
	Live        bool
	Stale       bool
	ObservedAt  time.Time
	EdgeKnown   bool
	EdgeOnline  bool
}

func (r ServiceCatalogRow) Scope() string {
	if r.Service.PublicName != "" {
		return "public"
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
	if !r.Service.Healthy {
		return "unhealthy"
	}
	if r.Service.PublicName != "" {
		if !r.EdgeKnown {
			return "edge-unknown"
		}
		if !r.EdgeOnline {
			return "edge-offline"
		}
	}
	return "healthy"
}

type serviceQueryResult struct {
	host     HostRecord
	snapshot remoteServiceSnapshot
	err      error
}

type edgeQueryResult struct {
	host   HostRecord
	routes []protocol.EdgeRouteInfo
	err    error
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
func CollectServiceCatalog(ctx context.Context, hosts []HostRecord, timeout time.Duration, query serviceQuery, queryEdge edgeQuery, cache serviceCatalogCache) ([]ServiceCatalogRow, map[string]error, error) {
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
	rowsByHost := make(map[string][]ServiceCatalogRow, len(hosts))
	rowCount := 0
	for _, host := range hosts {
		rowsByHost[host.ID] = cachedServiceCatalogRows(host, cached[host.ID])
		rowCount += len(rowsByHost[host.ID])
	}

	serviceResults := make(chan serviceQueryResult, len(hosts))
	edgeResults := make(chan edgeQueryResult, len(hosts))
	cacheResults := make(chan serviceCacheResult, len(hosts))
	serviceSemaphore := make(chan struct{}, maximumConcurrentServiceQueries)
	edgeSemaphore := make(chan struct{}, maximumConcurrentEdgeQueries)
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
		if queryEdge != nil {
			go func() {
				if !acquireQuery(operationCtx, edgeSemaphore) {
					return
				}
				routes, queryErr := queryEdge(operationCtx, host)
				<-edgeSemaphore
				select {
				case edgeResults <- edgeQueryResult{host: host, routes: routes, err: queryErr}:
				case <-operationCtx.Done():
				}
			}()
		}
	}

	diagnostics := make(map[string]error)
	edgeSnapshots := make(map[string][]protocol.EdgeRouteInfo)
	completedServices := make(map[string]bool, len(hosts))
	wantServices := len(hosts)
	wantEdges := 0
	wantCacheWrites := 0
	if queryEdge != nil {
		wantEdges = len(hosts)
	}
	for wantServices > 0 || wantEdges > 0 || wantCacheWrites > 0 {
		select {
		case result := <-serviceResults:
			wantServices--
			completedServices[result.host.ID] = true
			if result.err != nil {
				diagnostics[result.host.Alias] = result.err
				continue
			}
			newCount := rowCount - len(rowsByHost[result.host.ID]) + len(result.snapshot.Services)
			if newCount > storage.MaximumCachedServices {
				return nil, nil, fmt.Errorf("service catalog exceeds %d rows", storage.MaximumCachedServices)
			}
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
		case result := <-edgeResults:
			wantEdges--
			if result.err == nil {
				edgeSnapshots[result.host.Alias] = result.routes
			}
		case result := <-cacheResults:
			wantCacheWrites--
			if result.err != nil && operationCtx.Err() == nil {
				diagnostics[result.host.Alias] = catalogCacheWarning{err: result.err}
			}
		case <-operationCtx.Done():
			wantServices = 0
			wantEdges = 0
			wantCacheWrites = 0
		}
	}
	cancel()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	edgeStatus := firstEdgeSnapshot(edgeSnapshots)
	rows := make([]ServiceCatalogRow, 0)
	for _, host := range hosts {
		for _, row := range rowsByHost[host.ID] {
			if row.Service.PublicName != "" && edgeStatus != nil {
				status, exists := edgeStatus[row.Service.PublicName+"\x00"+row.Service.Name]
				row.EdgeKnown = true
				row.EdgeOnline = exists && status.Online
			}
			rows = append(rows, row)
			if len(rows) > storage.MaximumCachedServices {
				return nil, nil, fmt.Errorf("service catalog exceeds %d rows", storage.MaximumCachedServices)
			}
		}
		if _, exists := diagnostics[host.Alias]; !exists && !completedServices[host.ID] {
			diagnostics[host.Alias] = context.DeadlineExceeded
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Host.Alias != rows[j].Host.Alias {
			return rows[i].Host.Alias < rows[j].Host.Alias
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
				Name: row.Service.Name, Kind: string(row.Service.Kind), Target: row.Service.Target,
				PublicName: row.Service.PublicName, WakeOnRequest: row.Service.WakeOnRequest, Isolate: row.Service.Isolate,
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
		rows[index] = ServiceCatalogRow{Host: host, PrivateName: snapshot.PrivateName, Service: service, Live: true}
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

func firstEdgeSnapshot(snapshots map[string][]protocol.EdgeRouteInfo) map[string]protocol.EdgeRouteInfo {
	if len(snapshots) == 0 {
		return nil
	}
	aliases := make([]string, 0, len(snapshots))
	for alias := range snapshots {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	result := make(map[string]protocol.EdgeRouteInfo, len(snapshots[aliases[0]]))
	for _, route := range snapshots[aliases[0]] {
		result[route.PublicName+"\x00"+route.ServiceName] = route
	}
	return result
}

func catalogCandidates(rows []ServiceCatalogRow, route, hostAlias string) []ServiceCatalogRow {
	candidates := make([]ServiceCatalogRow, 0)
	for _, row := range rows {
		if row.Service.Name != route {
			continue
		}
		if hostAlias != "" && !strings.EqualFold(row.Host.Alias, hostAlias) {
			continue
		}
		candidates = append(candidates, row)
	}
	return candidates
}
