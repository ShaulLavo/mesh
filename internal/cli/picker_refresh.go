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

func (a *application) refreshPickerSessions(ctx context.Context, host HostRecord, cache CatalogCache) (HostSessions, error) {
	refreshed, err := CollectHostSessions(ctx, []HostRecord{host}, defaultCatalogTimeout, a.queryHost, cache)
	if err != nil {
		return HostSessions{}, err
	}
	return refreshed[0], nil
}

func (a *application) refreshPickerServices(ctx context.Context, host HostRecord, cache pickerServiceCache) *PickerServiceCatalog {
	operationContext, cancel := context.WithTimeout(ctx, defaultCatalogTimeout)
	defer cancel()
	cached, cacheErr := cache.LoadServices(operationContext, host)
	snapshot, queryErr := listRemoteServices(operationContext, host, a.dependencies.DialControl)
	if queryErr == nil {
		rows := liveServiceCatalogRows(snapshot.Host, snapshot)
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
