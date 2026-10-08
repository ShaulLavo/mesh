package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func pickerActionFixtureService() protocol.ServiceInfo {
	return protocol.ServiceInfo{Name: "dev", DisplayName: "Fregat dev", Kind: "proxy", Target: "3000", Healthy: true,
		Run:    &protocol.ServiceRun{Command: "fixture", Cwd: "/fixture", IdleMillis: 1000, ReadyTimeoutMillis: 1000},
		Demand: &protocol.ServiceDemand{State: protocol.DemandRunning}}
}

func TestPickerServiceActionsRouteToExactHostAndSettle(t *testing.T) {
	hosts := []HostRecord{{ID: "first", MachineName: "alpha", MeshIdentity: "a"}, {ID: "second", MachineName: "beta", MeshIdentity: "b", Endpoint: "wss://beta.example.test/control/ws"}}
	for _, action := range []PickerServiceAction{PickerStopService, PickerRestartService, PickerPingService, PickerOpenService} {
		t.Run(serviceActionTestName(action), func(t *testing.T) {
			calls := []string{}
			opened := ""
			service := pickerActionFixtureService()
			dial := func(ctx context.Context, host HostRecord) (transport.Conn, error) {
				if host.ID != "second" {
					t.Errorf("action used host %s", host.ID)
				}
				return serviceRemoteDial(host, func(request protocol.Control) protocol.Control {
					calls = append(calls, request.Type)
					switch request.Type {
					case protocol.TypeServiceList:
						return protocol.Control{Type: protocol.TypeServiceListed, Services: []protocol.ServiceInfo{service}}
					case protocol.TypeServiceStop:
						service.Demand = &protocol.ServiceDemand{State: protocol.DemandStopped}
					case protocol.TypeServiceStart:
						service.Demand = &protocol.ServiceDemand{State: protocol.DemandRunning}
					default:
						t.Errorf("unexpected control %s", request.Type)
					}
					if request.ServiceName != "dev" {
						t.Errorf("mutation names %q", request.ServiceName)
					}
					return protocol.Control{Type: protocol.TypeOK, ServiceName: "dev", Service: &service}
				})(ctx, host)
			}
			result, err := pickerServiceAction(t.Context(), hosts, dial, func(_ context.Context, address string) error { opened = address; return nil }, PickerServiceActionRequest{HostID: "second", ServiceName: "dev", Action: action})
			if err != nil || result.Row.Host.ID != "second" || result.Row.Service.Name != "dev" {
				t.Fatalf("action result %+v, %v", result, err)
			}
			want := []string{protocol.TypeServiceList}
			switch action {
			case PickerStopService:
				want = append(want, protocol.TypeServiceStop)
				if result.Row.Service.Demand.State != protocol.DemandStopped {
					t.Fatal("stop did not settle")
				}
			case PickerRestartService:
				want = append(want, protocol.TypeServiceStop, protocol.TypeServiceStart)
				if result.Row.Service.Demand.State != protocol.DemandRunning {
					t.Fatal("restart did not settle")
				}
			case PickerPingService:
				if result.Latency <= 0 || result.Row.Health() != "healthy" {
					t.Fatalf("ping result %+v", result)
				}
			case PickerOpenService:
				if opened != "https://beta.example.test/dev" {
					t.Fatalf("opened %q", opened)
				}
			}
			if action == PickerOpenService {
				if !reflect.DeepEqual(calls, want) {
					t.Fatalf("open calls %v", calls)
				}
				return
			}
			if opened != "" || !reflect.DeepEqual(calls, want) {
				t.Fatalf("calls %v, want %v; opened %q", calls, want, opened)
			}
		})
	}
}

func serviceActionTestName(action PickerServiceAction) string {
	return map[PickerServiceAction]string{PickerStopService: "stop", PickerRestartService: "restart", PickerPingService: "ping", PickerOpenService: "open"}[action]
}

func TestPickerServiceOpenUsesInvokingMachineURL(t *testing.T) {
	host := HostRecord{ID: "host", MachineName: "alpha", MeshIdentity: "key", Endpoint: "wss://alpha.example.test/control/ws"}
	for _, example := range []struct {
		name    string
		service protocol.ServiceInfo
		want    string
		fail    bool
	}{
		{name: "tailnet", service: protocol.ServiceInfo{Name: "dev", Kind: "proxy", Target: "3000", Healthy: true}, want: "https://alpha.example.test/dev"},
		{name: "public", service: protocol.ServiceInfo{Name: "dev", PublicName: "public.mesh.test", Kind: "proxy", Target: "3000", Healthy: true}, want: "https://public.mesh.test/dev"},
		{name: "remote loopback", service: protocol.ServiceInfo{Name: "5173", Kind: "proxy", Target: "3000", LocalOnly: true, Listens: []protocol.ServiceListen{{Public: 5173, Upstream: 3000}}, Healthy: true}, fail: true},
	} {
		t.Run(example.name, func(t *testing.T) {
			opened := ""
			dial := serviceRemoteDial(host, func(request protocol.Control) protocol.Control {
				return protocol.Control{Type: protocol.TypeServiceListed, Services: []protocol.ServiceInfo{example.service}}
			})
			_, err := pickerServiceAction(t.Context(), []HostRecord{host}, dial, func(_ context.Context, address string) error { opened = address; return nil }, PickerServiceActionRequest{HostID: host.ID, ServiceName: example.service.Name, Action: PickerOpenService})
			if example.fail {
				if err == nil || opened != "" {
					t.Fatalf("remote loopback opened %q, %v", opened, err)
				}
				return
			}
			if err != nil || opened != example.want {
				t.Fatalf("opened %q, %v; want %q", opened, err, example.want)
			}
		})
	}
	local := ServiceCatalogRow{Host: HostRecord{ID: localHostID(), MeshIdentity: localHostID(), local: true}, Service: protocol.ServiceInfo{Name: "5173", LocalOnly: true}}
	if address, err := pickerReachableURL(local); err != nil || address != "http://127.0.0.1:5173/" {
		t.Fatalf("local loopback %q, %v", address, err)
	}
}

func TestPickerURLFakesSelectOSAndExposeFailure(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		called := false
		err := runPickerOpener(t.Context(), goos, "https://fixture.example.test/dev", func(_ context.Context, name, address string) error {
			called = true
			want := "xdg-open"
			if goos == "darwin" {
				want = "open"
			}
			if name != want || address != "https://fixture.example.test/dev" {
				t.Fatalf("opener %s %s", name, address)
			}
			return errors.New("fixture opener unavailable")
		})
		if err == nil || called != (goos != "windows") {
			t.Fatalf("%s opener called=%v err=%v", goos, called, err)
		}
	}
}

func TestPickerServiceActionsKeepIdentityPinAndRejectBadAcknowledgement(t *testing.T) {
	host := HostRecord{ID: "host", MachineName: "alpha", MeshIdentity: "key"}
	for _, mode := range []string{"wrong host", "wrong route", "no run", "restart failure", "missing route"} {
		t.Run(mode, func(t *testing.T) {
			service := pickerActionFixtureService()
			calls := []string{}
			dial := reviewControlDial(t, func(_ HostRecord, request protocol.Control) *protocol.Control {
				calls = append(calls, request.Type)
				if request.Type == protocol.TypeHostInfo {
					id := host.ID
					if mode == "wrong host" {
						id = "intruder"
					}
					return &protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: id, MeshIdentity: host.MeshIdentity}}
				}
				if request.Type == protocol.TypeServiceList {
					if mode == "no run" {
						service.Run = nil
						service.Demand = nil
					}
					services := []protocol.ServiceInfo{service}
					if mode == "missing route" {
						services = nil
					}
					return &protocol.Control{Type: protocol.TypeServiceListed, Services: services}
				}
				if request.Type == protocol.TypeServiceStart {
					return &protocol.Control{Type: protocol.TypeError, Message: "fixture failed to start"}
				}
				name := "dev"
				if mode == "wrong route" {
					name = "another"
				}
				return &protocol.Control{Type: protocol.TypeOK, ServiceName: name, Service: &service}
			})
			_, err := pickerServiceAction(t.Context(), []HostRecord{host}, dial, func(context.Context, string) error { t.Fatal("opened during restart"); return nil }, PickerServiceActionRequest{HostID: host.ID, ServiceName: "dev", Action: PickerRestartService})
			if err == nil {
				t.Fatalf("%s accepted; calls=%v", mode, calls)
			}
			if mode == "wrong host" && len(calls) != 1 {
				t.Fatalf("identity mismatch sent operation %v", calls)
			}
		})
	}
}

func TestPickerServicePingUnhealthyIsASettledResult(t *testing.T) {
	host := HostRecord{ID: "host", MachineName: "alpha", MeshIdentity: "key"}
	service := pickerActionFixtureService()
	service.Healthy = false
	service.Problem = "fixture refused"
	dial := serviceRemoteDial(host, func(protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeServiceListed, Services: []protocol.ServiceInfo{service}}
	})
	result, err := pickerServiceAction(t.Context(), []HostRecord{host}, dial, nil, PickerServiceActionRequest{HostID: host.ID, ServiceName: "dev", Action: PickerPingService})
	if err != nil || result.Row.Health() != "unhealthy" || result.Latency <= 0 || result.Latency > time.Second {
		t.Fatalf("ping=%+v %v", result, err)
	}
}

func TestPickerServiceActionsRejectUnknownHostAndAction(t *testing.T) {
	for _, request := range []PickerServiceActionRequest{{HostID: "missing", ServiceName: "dev", Action: PickerPingService}, {HostID: "host", ServiceName: "dev", Action: 99}, {HostID: "host", ServiceName: "../bad", Action: PickerStopService}} {
		_, err := pickerServiceAction(t.Context(), []HostRecord{{ID: "host"}}, func(context.Context, HostRecord) (transport.Conn, error) {
			t.Fatal("invalid selection dialed")
			return nil, nil
		}, nil, request)
		if err == nil || strings.Contains(err.Error(), "<nil>") {
			t.Fatalf("invalid selection=%v", err)
		}
	}
}

func TestPickerPingHonorsVerifiedHealthCapability(t *testing.T) {
	host := HostRecord{ID: "alpha", MachineName: "alpha", MeshIdentity: "a"}
	for _, example := range []struct {
		name               string
		supported, unknown bool
		want               string
	}{
		{"legacy", false, false, "unknown"},
		{"unknown", true, true, "unknown"},
		{"healthy", true, false, "healthy"},
	} {
		t.Run(example.name, func(t *testing.T) {
			dial := reviewControlDial(t, func(host HostRecord, request protocol.Control) *protocol.Control {
				if request.Type == protocol.TypeHostInfo {
					return &protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: host.ID, MeshIdentity: host.MeshIdentity, ServiceHealthSupported: example.supported}}
				}
				if request.Type == protocol.TypeServiceList {
					return &protocol.Control{Type: protocol.TypeServiceListed, Services: []protocol.ServiceInfo{{Name: "dev", Kind: "proxy", Target: "3000", Healthy: !example.unknown, HealthUnknown: example.unknown}}}
				}
				return nil
			})
			result, err := pickerServiceAction(t.Context(), []HostRecord{host}, dial, nil, PickerServiceActionRequest{HostID: host.ID, ServiceName: "dev", Action: PickerPingService})
			if err != nil || result.Row.Health() != example.want {
				t.Fatalf("ping health=%s want=%s err=%v", result.Row.Health(), example.want, err)
			}
		})
	}
}
