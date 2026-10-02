package cli

import (
	"fmt"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
)

func TestDashboardServiceReadyRequiresRunningDemand(t *testing.T) {
	cases := []struct {
		name          string
		service       protocol.ServiceInfo
		ready, failed int
		state         string
	}{
		{name: "health not observed", service: protocol.ServiceInfo{HealthUnknown: true}, state: "unknown"},
		{name: "health not observed with demand failure", service: protocol.ServiceInfo{HealthUnknown: true, Demand: &protocol.ServiceDemand{State: protocol.DemandFailed, Failure: "process exited"}}, failed: 1, state: "unhealthy"},
		{name: "static healthy", service: protocol.ServiceInfo{Healthy: true}, ready: 1, state: "ready"},
		{name: "static unhealthy", service: protocol.ServiceInfo{}, failed: 1, state: "unhealthy"},
		{name: "listener-only healthy", service: protocol.ServiceInfo{Healthy: true, Demand: &protocol.ServiceDemand{}}, ready: 1, state: "ready"},
		{name: "listener-only unhealthy", service: protocol.ServiceInfo{Demand: &protocol.ServiceDemand{}}, failed: 1, state: "unhealthy"},
		{name: "static reported problem", service: protocol.ServiceInfo{Healthy: true, Problem: "probe failed"}, failed: 1, state: "unhealthy"},
		{name: protocol.DemandStopped, service: protocol.ServiceInfo{Healthy: true, Demand: &protocol.ServiceDemand{State: protocol.DemandStopped}}, state: "idle"},
		{name: protocol.DemandStarting, service: protocol.ServiceInfo{Healthy: true, Demand: &protocol.ServiceDemand{State: protocol.DemandStarting}}, state: "starting"},
		{name: protocol.DemandRunning, service: protocol.ServiceInfo{Healthy: true, Demand: &protocol.ServiceDemand{State: protocol.DemandRunning}}, ready: 1, state: "running"},
		{name: protocol.DemandStopping, service: protocol.ServiceInfo{Healthy: true, Demand: &protocol.ServiceDemand{State: protocol.DemandStopping}}, state: "stopping"},
		{name: protocol.DemandFailed, service: protocol.ServiceInfo{Healthy: true, Demand: &protocol.ServiceDemand{State: protocol.DemandFailed}}, failed: 1, state: "unhealthy"},
		{name: "running unhealthy", service: protocol.ServiceInfo{Demand: &protocol.ServiceDemand{State: protocol.DemandRunning}}, failed: 1, state: "unhealthy"},
	}
	for _, example := range cases {
		t.Run(example.name, func(t *testing.T) {
			serviceRow := projectDashboardService(example.service, true)
			if serviceRow.State != example.state {
				t.Errorf("state %q; want %q", serviceRow.State, example.state)
			}
			if example.service.Demand != nil && example.service.Demand.State == "" && serviceRow.State == "" {
				t.Fatalf("listener-only service state is blank: %+v", serviceRow)
			}
			rows := make([]protocol.ServiceInfo, dashboardServiceLimit)
			for index := range rows {
				rows[index] = protocol.ServiceInfo{Name: fmt.Sprintf("ready-%d", index), Healthy: true}
			}
			service := example.service
			service.Name = "beyond row cap"
			rows = append(rows, service)
			catalog := projectDashboardServices(rows, ObservedSection{}, true)
			idle := 0
			if example.state == "idle" {
				idle = 1
			}
			if catalog.Idle != idle {
				t.Error("idle totals beyond the row cap", catalog)
			}
			if catalog.Ready != dashboardServiceLimit+example.ready || catalog.Failed != example.failed || catalog.Total != len(rows) || len(catalog.Rows) != dashboardServiceLimit {
				t.Fatalf("readiness totals must reflect actual state beyond the displayed row cap: %+v", catalog)
			}
		})
	}
}
