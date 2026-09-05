package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

type pickerServiceCacheStub struct {
	rows       []storage.CachedService
	loadErr    error
	saveErr    error
	loadedHost HostRecord
	saved      []protocol.ServiceInfo
	saveCalls  int
}

func (c *pickerServiceCacheStub) LoadServices(_ context.Context, host HostRecord) ([]storage.CachedService, error) {
	c.loadedHost = host
	return append([]storage.CachedService(nil), c.rows...), c.loadErr
}

func (c *pickerServiceCacheStub) SaveServices(_ context.Context, _ HostRecord, _ string, rows []protocol.ServiceInfo) error {
	c.saveCalls++
	c.saved = append([]protocol.ServiceInfo(nil), rows...)
	return c.saveErr
}

func TestPickerServiceRefreshUsesOnlyTheSelectedHostsCache(t *testing.T) {
	host := setupCommandTestHost(t)
	host.services = []protocol.ServiceInfo{{Name: "blog", Kind: "proxy", Target: "3000", Healthy: true}}
	cache := &pickerServiceCacheStub{saveErr: errors.New("disk busy")}
	app := &application{dependencies: Dependencies{DialControl: host.dial}}

	refreshed := app.refreshPickerServices(context.Background(), host.host, cache)
	if cache.loadedHost.ID != host.host.ID {
		t.Fatalf("loaded cache for host %q, want %q", cache.loadedHost.ID, host.host.ID)
	}
	if refreshed == nil || refreshed.Stale || len(refreshed.Rows) != 1 || refreshed.Rows[0].URL() != "https://pc.mesh.shaulavo.dev/blog" {
		t.Fatalf("live service refresh = %#v", refreshed)
	}
	if len(cache.saved) != 1 || cache.saved[0].Name != "blog" {
		t.Fatalf("saved services = %#v", cache.saved)
	}
}

func TestPickerServiceRefreshDistinguishesCachedEmptyAndUnavailable(t *testing.T) {
	host := HostRecord{Alias: "pc", ID: "host-id", MeshIdentity: "host-key"}
	offline := func(context.Context, HostRecord) (transport.Conn, error) {
		return nil, errors.New("offline")
	}
	app := &application{dependencies: Dependencies{DialControl: offline}}
	cached := storage.CachedService{
		HostID: storage.HostID(host.ID), PrivateName: "pc.mesh.example",
		Service: meshserve.Service{Name: "blog", Kind: meshserve.Proxy, Target: "3000"}, Healthy: true,
	}

	refreshed := app.refreshPickerServices(context.Background(), host, &pickerServiceCacheStub{rows: []storage.CachedService{cached}})
	if refreshed == nil || !refreshed.Stale || len(refreshed.Rows) != 1 || refreshed.Rows[0].Health() != "offline/stale" {
		t.Fatalf("offline cached refresh = %#v", refreshed)
	}
	refreshed = app.refreshPickerServices(context.Background(), host, &pickerServiceCacheStub{})
	if refreshed == nil || !refreshed.Stale || len(refreshed.Rows) != 0 {
		t.Fatalf("offline empty refresh = %#v", refreshed)
	}
	refreshed = app.refreshPickerServices(context.Background(), host, &pickerServiceCacheStub{loadErr: errors.New("database closed")})
	if refreshed != nil {
		t.Fatalf("unavailable refresh = %#v, want nil", refreshed)
	}
}

func TestPickerServiceRefreshTreatsLiveEmptyAsAuthoritative(t *testing.T) {
	host := setupCommandTestHost(t)
	cache := &pickerServiceCacheStub{rows: []storage.CachedService{{
		HostID: storage.HostID(host.host.ID), PrivateName: "pc.mesh.shaulavo.dev",
		Service: meshserve.Service{Name: "old", Kind: meshserve.Proxy, Target: "3000"},
	}}}
	app := &application{dependencies: Dependencies{DialControl: host.dial}}

	refreshed := app.refreshPickerServices(context.Background(), host.host, cache)
	if refreshed == nil || refreshed.Stale || len(refreshed.Rows) != 0 {
		t.Fatalf("live empty refresh = %#v", refreshed)
	}
	if cache.saveCalls != 1 || len(cache.saved) != 0 {
		t.Fatalf("live empty save calls = %d, rows = %#v", cache.saveCalls, cache.saved)
	}
}
