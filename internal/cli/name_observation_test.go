package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

func TestOwnerNameObservationWatchProjectionKeepsIndependentFreshness(t *testing.T) {
	_, owner := controlFixtureAuthentication(t)
	host := HostRecord{ID: owner, MeshIdentity: owner, local: true}
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	view := StateView{Connection: StateReachable, Sections: map[string]ObservedSection{}}
	declaration := protocol.HostInfo{ID: owner, MeshIdentity: owner, MachineName: "fixture-owner", NameRevision: 2}
	initial := protocol.Control{Type: protocol.TypeStateSnapshot, StateSnapshot: &protocol.StateSnapshot{
		Seq: 1, Host: &declaration, Current: map[string]protocol.Observation{protocol.TopicHost: {}},
	}}
	if err := applyVerifiedState(t.Context(), host, &view, initial, now, 0); err != nil {
		t.Fatal(err)
	}
	assertLabel := func(retained bool) {
		t.Helper()
		projected := projectDashboardState(DashboardHost{ID: owner}, view)
		if projected.Host.MachineName != "fixture-owner" || projected.Host.NameRevision != 2 || !projected.Host.NameVerified {
			t.Fatal("watch projection lost the owner declaration")
		}
		if strings.Contains(projected.NameLabel(now), "last known name") != retained {
			t.Fatalf("owner observation label retained=%v, want %v", projected.NameLabel(now), retained)
		}
	}
	assertLabel(false)
	now = now.Add(31 * time.Second)
	applyCurrent := func(age int64, failing bool) {
		t.Helper()
		message := protocol.Control{Type: protocol.TypeStateCurrent, StateCurrent: &protocol.StateCurrent{
			Seq: view.Seq + 1, Sections: map[string]protocol.Observation{protocol.TopicHost: {AgeMillis: age, Failing: failing}},
		}}
		if err := applyVerifiedState(t.Context(), host, &view, message, now, 0); err != nil {
			t.Fatal(err)
		}
	}
	applyCurrent(31000, false)
	assertLabel(true)
	if !view.LastReply.Equal(now) {
		t.Fatal("stale owner observation did not keep transport reply fresh")
	}
	applyCurrent(0, false)
	assertLabel(false)
	applyCurrent(0, true)
	assertLabel(true)
	applyCurrent(0, false)
	assertLabel(false)
	view.Connection = StateUnreachable
	assertLabel(true)
	view.Connection = StateReachable
	now = now.Add(31 * time.Second)
	assertLabel(true)
}
