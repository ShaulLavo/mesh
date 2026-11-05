package daemon

import (
	"testing"

	"github.com/shaul/mesh/internal/protocol"
)

// Live snapshots copy the whole recipe and status, so a kept lifetime and a
// hold survive every copy, and a change of hold is a change of health.
func TestKeptStateSurvivesClonesAndHealthComparison(t *testing.T) {
	service := protocol.ServiceInfo{
		Name: "ai", Kind: "proxy", Target: "8317",
		Run:    &protocol.ServiceRun{Command: "./run", Cwd: "/srv/proxy", ReadyTimeoutMillis: 60_000, KeepRunning: true},
		Demand: &protocol.ServiceDemand{State: protocol.DemandStopped, SessionID: "7K3D", Held: true},
	}
	cloned := cloneStateService(service)
	if cloned.Run == service.Run || cloned.Demand == service.Demand || !cloned.Run.KeepRunning || !cloned.Demand.Held {
		t.Fatalf("clone = %+v / %+v, want owned copies keeping the lifetime and hold", cloned.Run, cloned.Demand)
	}
	released := *service.Demand
	released.Held = false
	if sameDemandHealth(service.Demand, &released) {
		t.Fatal("releasing a hold kept the old health observation")
	}
	if !sameDemandHealth(service.Demand, cloned.Demand) {
		t.Fatal("an identical held status reads as a health change")
	}
}
