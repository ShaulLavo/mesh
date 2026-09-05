package cli

import (
	"context"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
)

type pickerServiceCache interface {
	LoadServices(context.Context, HostRecord) ([]storage.CachedService, error)
	SaveServices(context.Context, HostRecord, string, []protocol.ServiceInfo) error
}

type pickerCatalogCache interface {
	CatalogCache
	pickerServiceCache
}

type pickerSessionResult struct {
	catalog HostSessions
	err     error
}

type pickerServiceResult struct {
	catalog *PickerServiceCatalog
}

func (a *application) refreshPickerSessions(ctx context.Context, host HostRecord, cache CatalogCache) (HostSessions, error) {
	refreshed, err := CollectHostSessions(ctx, []HostRecord{host}, defaultCatalogTimeout, a.queryHost, cache)
	if err != nil {
		return HostSessions{}, err
	}
	return refreshed[0], nil
}

// refreshPickerHost reads both sides of the open-host panel concurrently. A
// service failure never discards a fresh session catalog, and a slow service
// query cannot add another catalog timeout to session discovery.
func (a *application) refreshPickerHost(ctx context.Context, host HostRecord, cache pickerCatalogCache) (PickerHostSnapshot, error) {
	sessionResults := make(chan pickerSessionResult, 1)
	serviceResults := make(chan pickerServiceResult, 1)
	go func() {
		catalog, err := a.refreshPickerSessions(ctx, host, cache)
		sessionResults <- pickerSessionResult{catalog: catalog, err: err}
	}()
	go func() {
		serviceResults <- pickerServiceResult{catalog: a.refreshPickerServices(ctx, host, cache)}
	}()

	sessions := <-sessionResults
	services := <-serviceResults
	if sessions.err != nil {
		return PickerHostSnapshot{}, sessions.err
	}
	return PickerHostSnapshot{Sessions: sessions.catalog, Services: services.catalog}, nil
}

func (a *application) refreshPickerServices(ctx context.Context, host HostRecord, cache pickerServiceCache) *PickerServiceCatalog {
	operationContext, cancel := context.WithTimeout(ctx, defaultCatalogTimeout)
	defer cancel()
	cached, cacheErr := cache.LoadServices(operationContext, host)
	snapshot, queryErr := listRemoteServices(operationContext, host, a.dependencies.DialControl)
	if queryErr == nil {
		rows := liveServiceCatalogRows(host, snapshot)
		cacheContext, cancelCache := context.WithTimeout(operationContext, serviceCacheWriteTimeout)
		_ = cache.SaveServices(cacheContext, host, snapshot.PrivateName, snapshot.Services)
		cancelCache()
		return &PickerServiceCatalog{Rows: rows}
	}
	if cacheErr != nil {
		return nil
	}
	return &PickerServiceCatalog{Rows: cachedServiceCatalogRows(host, cached), Stale: true}
}
