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
	}{
		{name: "static healthy", service: protocol.ServiceInfo{Healthy: true}, ready: 1},
		{name: "static unhealthy", service: protocol.ServiceInfo{}, failed: 1},
		{name: "static reported problem", service: protocol.ServiceInfo{Healthy: true, Problem: "probe failed"}, failed: 1},
		{name: protocol.DemandStopped, service: protocol.ServiceInfo{Healthy: true, Demand: &protocol.ServiceDemand{State: protocol.DemandStopped}}},
		{name: protocol.DemandStarting, service: protocol.ServiceInfo{Healthy: true, Demand: &protocol.ServiceDemand{State: protocol.DemandStarting}}},
		{name: protocol.DemandRunning, service: protocol.ServiceInfo{Healthy: true, Demand: &protocol.ServiceDemand{State: protocol.DemandRunning}}, ready: 1},
		{name: protocol.DemandStopping, service: protocol.ServiceInfo{Healthy: true, Demand: &protocol.ServiceDemand{State: protocol.DemandStopping}}},
		{name: protocol.DemandFailed, service: protocol.ServiceInfo{Healthy: true, Demand: &protocol.ServiceDemand{State: protocol.DemandFailed}}, failed: 1},
		{name: "running unhealthy", service: protocol.ServiceInfo{Demand: &protocol.ServiceDemand{State: protocol.DemandRunning}}, failed: 1},
	}
	for _, example := range cases {
		t.Run(example.name, func(t *testing.T) {
			rows := make([]protocol.ServiceInfo, dashboardServiceLimit)
			for index := range rows {
				rows[index] = protocol.ServiceInfo{Name: fmt.Sprintf("ready-%d", index), Healthy: true}
			}
			service := example.service
			service.Name = "beyond row cap"
			rows = append(rows, service)
			catalog := projectDashboardServices(rows, ObservedSection{})
			if catalog.Ready != dashboardServiceLimit+example.ready || catalog.Failed != example.failed || catalog.Total != len(rows) || len(catalog.Rows) != dashboardServiceLimit {
				t.Fatalf("readiness totals must reflect actual state beyond the displayed row cap: %+v", catalog)
			}
		})
	}
}
