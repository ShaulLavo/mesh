package cli

import (
	"fmt"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

func TestDashboardMissingProducerHealthCapabilityIsUnknown(t *testing.T) {
	now := time.Now()
	state := StateView{Connection: StateReachable, LastReply: now, Services: []protocol.ServiceInfo{{Name: "proxy", Problem: "legacy definition health", Healthy: false}, {Name: "waiting", Healthy: true, Demand: &protocol.ServiceDemand{State: protocol.DemandStopped}}}, Sections: map[string]ObservedSection{protocol.TopicServices: {ReceivedAt: now}}}
	view := projectDashboardState(DashboardHost{ID: "host"}, state)
	if view.Services.Ready != 0 || view.Services.Failed != 0 || view.Services.Total != 2 || view.Services.Unknown != 2 {
		t.Fatalf("missing advertisement manufactured health facts: %+v", view.Services)
	}
	for _, row := range view.Services.Rows {
		if row.State != "health unknown" || row.Failed || row.Problem != "" {
			t.Fatalf("legacy health reached rows: %+v", row)
		}
	}
}

func TestDashboardHealthCapabilityAndUnknownCountsSurviveSnapshots(t *testing.T) {
	now := time.Now()
	for _, supported := range []bool{false, true} {
		state := StateView{Connection: StateReachable, ServiceHealthSupported: supported}
		rows := []protocol.ServiceInfo{{Name: "ready", Healthy: true}, {Name: "failed", Problem: "connection refused"}, {Name: "waiting", Healthy: true, Demand: &protocol.ServiceDemand{State: protocol.DemandStopped}}}
		for i := range 20 {
			rows = append(rows, protocol.ServiceInfo{Name: fmt.Sprint(i), HealthUnknown: true})
		}
		for seq := uint64(1); seq <= 2; seq++ {
			message := protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{Seq: seq, Services: rows, Current: map[string]protocol.Observation{protocol.TopicServices: {}}}}
			if err := state.Apply(message, now, 0); err != nil {
				t.Fatal(err)
			}
			if state.ServiceHealthSupported != supported {
				t.Fatal("verified producer capability lost on snapshot")
			}
			services := projectDashboardState(DashboardHost{}, state).Services
			if len(services.Rows) != dashboardServiceLimit || services.Total != len(rows) {
				t.Fatalf("service row cap/count: %+v", services)
			}
			if !supported {
				if services.Ready != 0 || services.Failed != 0 || services.Unknown != 23 {
					t.Fatalf("missing capability has health facts: %+v", services)
				}
				continue
			}
			if services.Ready != 1 || services.Failed != 1 || services.Unknown != 20 {
				t.Fatalf("new producer health facts lost beyond row cap: %+v", services)
			}
		}
	}
}
