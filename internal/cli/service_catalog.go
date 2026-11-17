package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
)

const (
	serviceHealthUnknown            = "unknown"
	maximumConcurrentServiceQueries = 16

	maximumConcurrentCacheWrites = 4
	serviceCacheWriteTimeout     = catalogCacheWriteTimeout
)

type serviceQuery func(context.Context, HostRecord) (remoteServiceSnapshot, error)

type serviceCatalogCache interface {
	LoadAllServices(context.Context, []HostRecord) (map[string][]storage.CachedService, error)
	SaveServices(context.Context, HostRecord, string, []protocol.ServiceInfo) (bool, error)
}

// ServiceCatalogRow is one live or cached private service definition.
type ServiceCatalogRow struct {
	Policy      domainpolicy.CatalogToken
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

// serviceCatalogCacheLoadCap is a hang cap on the local cache read, not a
// speed target: a slow disk must not fail a command that only reads it. Tests
// lower it to prove a blocked cache still returns.
var serviceCatalogCacheLoadCap = 5 * time.Second

// CollectServiceCatalog fans out one-shot live queries under one deadline and
// falls back to the complete cached list for each unavailable host. The
// timeout bounds remote discovery only; it starts after the local cache read.
func CollectServiceCatalog(ctx context.Context, hosts []HostRecord, timeout time.Duration, query serviceQuery, cache serviceCatalogCache) ([]ServiceCatalogRow, map[string]error, error) {
	if timeout <= 0 {
		return nil, nil, errors.New("cli: service catalog timeout must be positive")
	}
	return collectServiceCatalog(ctx, hosts, serviceCatalogCacheLoadCap, func() time.Time { return time.Now().Add(timeout) }, query, cache)
}

// collectServiceCatalogBy fits the cache read and remote discovery inside one
// hard deadline, for callers such as the agent API that promise one.
func collectServiceCatalogBy(ctx context.Context, hosts []HostRecord, deadline time.Time, query serviceQuery, cache serviceCatalogCache) ([]ServiceCatalogRow, map[string]error, error) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil, nil, errors.New("cli: service catalog deadline has passed")
	}
	return collectServiceCatalog(ctx, hosts, min(serviceCatalogCacheLoadCap, remaining), func() time.Time { return deadline }, query, cache)
}

// discovery is read after the cache load, so a relative timeout starts then.
// cachedServiceCatalog is the starting point every live answer replaces.
type cachedServiceCatalog struct {
	observedHosts map[string]HostRecord
	rowsByHost    map[string][]ServiceCatalogRow
	rowCount      int
}

func loadCachedServiceCatalog(ctx context.Context, hosts []HostRecord, loadCap time.Duration, query serviceQuery, cache serviceCatalogCache) (cachedServiceCatalog, error) {
	if ctx == nil {
		return cachedServiceCatalog{}, errors.New("cli: nil service catalog context")
	}
	if query == nil || cache == nil {
		return cachedServiceCatalog{}, errors.New("cli: incomplete service catalog dependencies")
	}
	if len(hosts) > maximumConfiguredHosts {
		return cachedServiceCatalog{}, fmt.Errorf("cli: host count %d exceeds %d", len(hosts), maximumConfiguredHosts)
	}
	loadCtx, loadCancel := context.WithTimeout(ctx, loadCap)
	cached, err := cache.LoadAllServices(loadCtx, hosts)
	loadCancel()
	if err != nil {
		return cachedServiceCatalog{}, fmt.Errorf("load cached services: %w", err)
	}
	catalog := cachedServiceCatalog{observedHosts: make(map[string]HostRecord, len(hosts)), rowsByHost: make(map[string][]ServiceCatalogRow, len(hosts))}
	for _, host := range hosts {
		catalog.observedHosts[host.ID] = host
		catalog.rowsByHost[host.ID] = cachedServiceCatalogRows(host, cached[host.ID])
		catalog.rowCount += len(catalog.rowsByHost[host.ID])
	}
	return catalog, nil
}

func collectServiceCatalog(ctx context.Context, hosts []HostRecord, loadCap time.Duration, discovery func() time.Time, query serviceQuery, cache serviceCatalogCache) ([]ServiceCatalogRow, map[string]error, error) {
	catalog, err := loadCachedServiceCatalog(ctx, hosts, loadCap, query, cache)
	if err != nil {
		return nil, nil, err
	}
	operationCtx, cancel := context.WithDeadline(ctx, discovery())
	defer cancel()
	run := newServiceCatalogRun(len(hosts), query, cache, catalog)
	for _, host := range hosts {
		go run.queryHost(operationCtx, host)
	}
	if err := run.collect(operationCtx, len(hosts)); err != nil {
		return nil, nil, err
	}
	cancel()
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("service catalog: %w", err)
	}
	rows, err := flattenServiceCatalog(hosts, catalog.rowsByHost, run.diagnostics, run.completed)
	if err != nil {
		return nil, nil, err
	}
	return finishServiceCatalog(ctx, hosts, catalog.observedHosts, rows), run.diagnostics, nil
}

// serviceCatalogRun owns one discovery: live answers replace cached rows and
// each answer is written back to the cache, all under one operation context.
type serviceCatalogRun struct {
	query            serviceQuery
	cache            serviceCatalogCache
	catalog          *cachedServiceCatalog
	serviceResults   chan serviceQueryResult
	cacheResults     chan serviceCacheResult
	serviceSemaphore chan struct{}
	cacheSemaphore   chan struct{}
	diagnostics      map[string]error
	completed        map[string]bool
	wantCacheWrites  int
}

func newServiceCatalogRun(hosts int, query serviceQuery, cache serviceCatalogCache, catalog cachedServiceCatalog) *serviceCatalogRun {
	return &serviceCatalogRun{query: query, cache: cache, catalog: &catalog,
		serviceResults: make(chan serviceQueryResult, hosts), cacheResults: make(chan serviceCacheResult, hosts),
		serviceSemaphore: make(chan struct{}, maximumConcurrentServiceQueries), cacheSemaphore: make(chan struct{}, maximumConcurrentCacheWrites),
		diagnostics: make(map[string]error), completed: make(map[string]bool, hosts)}
}

func (r *serviceCatalogRun) queryHost(ctx context.Context, host HostRecord) {
	if !acquireQuery(ctx, r.serviceSemaphore) {
		return
	}
	snapshot, err := r.query(ctx, host)
	<-r.serviceSemaphore
	select {
	case r.serviceResults <- serviceQueryResult{host: host, snapshot: snapshot, err: err}:
	case <-ctx.Done():
	}
}

func (r *serviceCatalogRun) saveHost(ctx context.Context, host HostRecord, privateName string, services []protocol.ServiceInfo) {
	if !acquireQuery(ctx, r.cacheSemaphore) {
		return
	}
	cacheCtx, cacheCancel := context.WithTimeout(ctx, serviceCacheWriteTimeout)
	_, err := r.cache.SaveServices(cacheCtx, host, privateName, services)
	cacheCancel()
	<-r.cacheSemaphore
	select {
	case r.cacheResults <- serviceCacheResult{host: host, err: err}:
	case <-ctx.Done():
	}
}

// collect waits for every query and every resulting cache write, or for the
// operation deadline, whichever comes first.
func (r *serviceCatalogRun) collect(ctx context.Context, wantServices int) error {
	for wantServices > 0 || r.wantCacheWrites > 0 {
		select {
		case result := <-r.serviceResults:
			wantServices--
			r.completed[result.host.ID] = true
			if result.err != nil {
				r.diagnostics[result.host.ID] = result.err
				continue
			}
			if err := r.acceptService(ctx, result); err != nil {
				return err
			}
		case result := <-r.cacheResults:
			r.wantCacheWrites--
			if result.err != nil && ctx.Err() == nil {
				r.diagnostics[result.host.ID] = catalogCacheWarning{err: result.err}
			}
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

func (r *serviceCatalogRun) acceptService(ctx context.Context, result serviceQueryResult) error {
	catalog := r.catalog
	newCount := catalog.rowCount - len(catalog.rowsByHost[result.host.ID]) + len(result.snapshot.Services)
	if newCount > storage.MaximumCachedServices {
		return fmt.Errorf("service catalog exceeds %d rows", storage.MaximumCachedServices)
	}
	if result.snapshot.Host.ID != "" {
		result.host = result.snapshot.Host
	}
	catalog.observedHosts[result.host.ID] = result.host
	catalog.rowsByHost[result.host.ID] = liveServiceCatalogRows(result.host, result.snapshot)
	catalog.rowCount = newCount
	r.wantCacheWrites++
	go r.saveHost(ctx, result.host, result.snapshot.PrivateName, append([]protocol.ServiceInfo(nil), result.snapshot.Services...))
	return nil
}

// flattenServiceCatalog orders rows by configured host and marks every host
// that neither answered nor failed as timed out.
func flattenServiceCatalog(hosts []HostRecord, rowsByHost map[string][]ServiceCatalogRow, diagnostics map[string]error, completed map[string]bool) ([]ServiceCatalogRow, error) {
	rows := make([]ServiceCatalogRow, 0)
	for _, host := range hosts {
		rows = append(rows, rowsByHost[host.ID]...)
		if len(rows) > storage.MaximumCachedServices {
			return nil, fmt.Errorf("service catalog exceeds %d rows", storage.MaximumCachedServices)
		}
		if _, exists := diagnostics[host.ID]; !exists && !completed[host.ID] {
			diagnostics[host.ID] = context.DeadlineExceeded
		}
	}
	return rows, nil
}

func finishServiceCatalog(ctx context.Context, hosts []HostRecord, observedHosts map[string]HostRecord, rows []ServiceCatalogRow) []ServiceCatalogRow {
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
	currentPolicy, policyErr := loadedServicePolicy(ctx)
	for index := range rows {
		if policyErr != nil || rows[index].Policy.Generation == 0 || rows[index].Policy != currentPolicy {
			rows[index].Live, rows[index].Stale = false, true
		}
	}
	return rows
}

func cachedServiceCatalogRows(host HostRecord, cached []storage.CachedService) []ServiceCatalogRow {
	rows := make([]ServiceCatalogRow, len(cached))
	for index, row := range cached {
		service := protocol.ServiceDefinitionInfo(row.Service)
		service.Healthy, service.Problem = row.Healthy, row.Problem
		rows[index] = ServiceCatalogRow{
			Policy: row.Policy, Host: host, PrivateName: row.PrivateName,
			Service: service,
			Stale:   true, ObservedAt: row.ObservedAt,
		}
	}
	return rows
}

func liveServiceCatalogRows(host HostRecord, snapshot remoteServiceSnapshot) []ServiceCatalogRow {
	rows := make([]ServiceCatalogRow, len(snapshot.Services))
	for index, service := range snapshot.Services {
		rows[index] = ServiceCatalogRow{Host: host, PrivateName: snapshot.PrivateName, Service: service, Policy: snapshot.Policy, Live: snapshot.Policy.Generation != 0, Stale: snapshot.Policy.Generation == 0, HealthUnsupported: !snapshot.ServiceHealthSupported}
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
