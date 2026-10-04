package cli

import (
	"testing"
	"time"

	"github.com/shaul/mesh/internal/hostmetrics"
	"github.com/shaul/mesh/internal/protocol"
)

func TestDashboardProjectionFirstMetricsPending(t *testing.T) {
	for _, failing := range []bool{false, true} {
		state := StateView{Connection: StateReachable, LastReply: time.Now(),
			Sections: map[string]ObservedSection{protocol.TopicMetrics: {Observation: protocol.Observation{Failing: failing}}}}
		view := projectDashboardState(DashboardHost{}, state)
		if view.CPU.State != "pending" || view.RAM.State != "pending" || view.Uptime.State != "pending" || view.Temperature.State != "pending" {
			t.Fatalf("no metrics payload must remain pending, failing=%v: %+v", failing, view)
		}
		if view.CPU.Failing != failing || view.RAM.Failing != failing {
			t.Fatalf("first metrics failure lost: %+v", view)
		}
		state.Metrics = &hostmetrics.Snapshot{PerformanceVersion: 1, CPU: hostmetrics.Reading[float64]{Availability: hostmetrics.Unavailable}}
		view = projectDashboardState(DashboardHost{}, state)
		if view.CPU.State != hostmetrics.Unavailable || view.PerformanceVersion != 1 {
			t.Fatalf("received unavailable reading mistaken for a pending payload: %+v", view)
		}
	}
}
