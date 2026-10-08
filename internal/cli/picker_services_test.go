package cli

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

func TestPickerFleetServicesWatchUsesCatalogAndPinnedStateWatch(t *testing.T) {
	hosts := []HostRecord{{ID: "alpha", MachineName: "alpha", MeshIdentity: "a"}, {ID: "beta", MachineName: "beta", MeshIdentity: "b"}}
	var watches, lists atomic.Int32
	dial := reviewControlDial(t, func(host HostRecord, request protocol.Control) *protocol.Control {
		switch request.Type {
		case protocol.TypeHostInfo:
			return &protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: host.ID, MeshIdentity: host.MeshIdentity, PrivateName: host.MachineName + ".mesh.mesh.test", ServiceHealthSupported: true}}
		case protocol.TypeStateWatch:
			watches.Add(1)
			if len(request.Watch.Topics) != 1 || request.Watch.Topics[0] != protocol.TopicServices {
				t.Error("picker requested unrelated watch topics")
			}
			return &protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1, Services: []protocol.ServiceInfo{{Name: "dev", DisplayName: "Fregat dev", Kind: "proxy", Target: "3000", Healthy: true}}, Current: map[string]protocol.Observation{protocol.TopicServices: {}}}}
		case protocol.TypeServiceList:
			lists.Add(1)
			t.Error("native watch polled service.list")
		}
		return nil
	})
	monitor := pickerServiceMonitor{hosts: hosts, watcher: NewStateWatcher(dial)}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	fresh := map[string]bool{}
	err := monitor.Run(ctx, func(update PickerServicesUpdate) {
		if update.Catalog.Stale || len(update.Catalog.Rows) == 0 {
			return
		}
		row := update.Catalog.Rows[0]
		if row.URL() != "https://"+update.Host.MachineName+".mesh.mesh.test/dev" || row.Service.DisplayName != "Fregat dev" || row.Host.ID != update.Host.ID {
			t.Errorf("fleet row %+v", row)
		}
		fresh[update.Host.ID] = true
		if len(fresh) == 2 {
			cancel()
		}
	})
	if err != nil || len(fresh) != 2 || watches.Load() != 2 || lists.Load() != 0 {
		t.Fatalf("fleet watch fresh=%v watches=%d lists=%d err=%v", fresh, watches.Load(), lists.Load(), err)
	}
}

func TestPickerFleetServicesWatchRetainsOfflineCacheAndReportsFailure(t *testing.T) {
	host := HostRecord{ID: "alpha", MachineName: "alpha", MeshIdentity: "a"}
	cache := &pickerServiceCacheStub{rows: []storage.CachedService{{HostID: storage.HostID(host.ID), PrivateName: "alpha.example.test", Service: meshserve.Service{Name: "dev", DisplayName: "Fregat dev", Kind: meshserve.Proxy, Target: "3000"}, Healthy: true}}}
	var calls atomic.Int32
	dial := func(context.Context, HostRecord) (transport.Conn, error) {
		calls.Add(1)
		return nil, errors.New("fixture offline")
	}
	monitor := pickerServiceMonitor{hosts: []HostRecord{host}, watcher: NewStateWatcher(dial), cache: cache}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	seen := 0
	err := monitor.Run(ctx, func(update PickerServicesUpdate) {
		seen++
		if !update.Catalog.Stale || len(update.Catalog.Rows) != 1 || update.Catalog.Rows[0].Live || update.Catalog.Rows[0].URL() != "https://alpha.example.test/dev" {
			t.Errorf("offline cache %+v", update)
		}
		if update.Problem != "" {
			cancel()
		}
	})
	if err != nil || seen != 2 || calls.Load() != 1 {
		t.Fatalf("offline seen=%d calls=%d err=%v", seen, calls.Load(), err)
	}
}

func TestPickerFleetServicesWatchCachesAuthoritativeEmpty(t *testing.T) {
	host := HostRecord{ID: "alpha", MachineName: "alpha", MeshIdentity: "a"}
	cache := &pickerServiceCacheStub{rows: []storage.CachedService{{HostID: storage.HostID(host.ID), Service: meshserve.Service{Name: "old", Kind: meshserve.Proxy, Target: "3000"}}}}
	dial := reviewControlDial(t, func(host HostRecord, request protocol.Control) *protocol.Control {
		if request.Type == protocol.TypeStateWatch {
			return &protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1, Current: map[string]protocol.Observation{protocol.TopicServices: {}}}}
		}
		return nil
	})
	monitor := pickerServiceMonitor{hosts: []HostRecord{host}, watcher: NewStateWatcher(dial), cache: cache}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	authoritative := false
	err := monitor.Run(ctx, func(update PickerServicesUpdate) {
		if !update.Catalog.Stale {
			authoritative = true
			cancel()
		}
	})
	if err != nil || !authoritative || cache.saveCalls != 1 || len(cache.saved) != 0 {
		t.Fatalf("empty cache authoritative=%v saved=%d rows=%v err=%v", authoritative, cache.saveCalls, cache.saved, err)
	}
}

func TestPickerServiceStateReusesDashboardHealth(t *testing.T) {
	for _, example := range []struct {
		name    string
		service protocol.ServiceInfo
		want    string
	}{
		{"healthy", protocol.ServiceInfo{Healthy: true}, "running"},
		{"unhealthy", protocol.ServiceInfo{Healthy: false}, "unhealthy"},
		{"idle", protocol.ServiceInfo{Healthy: true, Demand: &protocol.ServiceDemand{State: protocol.DemandStopped}}, "idle"},
		{"failed", protocol.ServiceInfo{Demand: &protocol.ServiceDemand{State: protocol.DemandFailed, Failure: "fixture refused"}}, "unhealthy"},
		{"unknown", protocol.ServiceInfo{HealthUnknown: true}, "unknown"},
	} {
		t.Run(example.name, func(t *testing.T) {
			if got := PickerServiceState(ServiceCatalogRow{Service: example.service, Live: true}); got != example.want {
				t.Fatalf("state=%s want=%s", got, example.want)
			}
		})
	}
}

func TestPickerServiceCallbacksWiredAndExpiredAfterReturn(t *testing.T) {
	fixture := setupCommandTestHost(t)
	fixture.services = []protocol.ServiceInfo{{Name: "dev", Kind: "proxy", Target: "3000", Healthy: true}}
	var captured PickerServiceActionFunc
	_, _, err := executeCommand(t, Dependencies{DialControl: fixture.dial, Picker: func(ctx context.Context, input PickerInput) (PickerSelection, error) {
		if input.WatchServices == nil || input.ServiceAction == nil {
			t.Fatal("service callbacks missing")
		}
		captured = input.ServiceAction
		result, err := input.ServiceAction(ctx, PickerServiceActionRequest{HostID: fixture.host.ID, ServiceName: "dev", Action: PickerPingService})
		if err != nil || result.Row.Service.Name != "dev" {
			t.Fatalf("wired ping %+v %v", result, err)
		}
		return PickerSelection{}, nil
	}})
	if err != nil || captured == nil {
		t.Fatalf("picker wire %v", err)
	}
	if _, err := captured(t.Context(), PickerServiceActionRequest{HostID: fixture.host.ID, ServiceName: "dev", Action: PickerPingService}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired callback %v", err)
	}
}

func TestPickerCachedLocalOnlyRoutePreservesScope(t *testing.T) {
	host := HostRecord{ID: "alpha", MachineName: "alpha", Endpoint: "wss://alpha.example.test/control/ws"}
	rows := cachedServiceCatalogRows(host, []storage.CachedService{{Service: meshserve.Service{Name: "5173", Kind: meshserve.Proxy, Target: "3000", LocalOnly: true}}})
	if len(rows) != 1 || rows[0].Scope() != "local" {
		t.Fatalf("cached loopback became a fleet URL: %+v", rows)
	}
	if _, err := pickerReachableURL(rows[0]); err == nil {
		t.Fatal("remote cached loopback has an invoking-machine URL")
	}
}

func TestPickerFleetServicesMissingProducerHealthIsUnknown(t *testing.T) {
	host := HostRecord{ID: "alpha", MachineName: "alpha", MeshIdentity: "a"}
	dial := reviewControlDial(t, func(host HostRecord, request protocol.Control) *protocol.Control {
		if request.Type == protocol.TypeStateWatch {
			return &protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: 1, Services: []protocol.ServiceInfo{
				{Name: "dev", Kind: "proxy", Target: "3000", Healthy: false},
				{Name: "waiting", Kind: "proxy", Target: "4000", Healthy: true, Demand: &protocol.ServiceDemand{State: protocol.DemandStopped}},
			}, Current: map[string]protocol.Observation{protocol.TopicServices: {}}}}
		}
		return nil
	})
	monitor := pickerServiceMonitor{hosts: []HostRecord{host}, watcher: NewStateWatcher(dial)}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	seen := false
	err := monitor.Run(ctx, func(update PickerServicesUpdate) {
		if update.Catalog.Stale {
			return
		}
		seen = true
		for _, row := range update.Catalog.Rows {
			if got := PickerServiceState(row); got != "unknown" {
				t.Errorf("producer without health capability reports %s", got)
			}
		}
		cancel()
	})
	if err != nil || !seen {
		t.Fatalf("health projection seen=%v err=%v", seen, err)
	}
}
