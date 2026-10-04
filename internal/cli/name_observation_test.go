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
		if strings.Contains(projected.NameLabel(now), "cached name") != retained {
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

func TestCachedNameKeepsObservationAndConnectionSeparate(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	fresh := DashboardHostView{Host: DashboardHost{ID: "owner", MachineName: "garden", NameVerified: true}, Connection: StateReachable, LastReply: now, NameObservedAt: now}
	cases := []struct {
		name   string
		change func(*DashboardHostView)
		cached bool
	}{
		{name: "fresh authenticated", change: func(*DashboardHostView) {}},
		{name: "retained unverified", change: func(h *DashboardHostView) { h.Host.NameVerified = false }, cached: true},
		{name: "fresh reply old name", change: func(h *DashboardHostView) { h.NameObservedAt = now.Add(-31 * time.Second) }, cached: true},
		{name: "missing name observation", change: func(h *DashboardHostView) { h.NameObservedAt = time.Time{} }, cached: true},
		{name: "failed name observation", change: func(h *DashboardHostView) { h.NameFailing = true }, cached: true},
		{name: "connecting", change: func(h *DashboardHostView) { h.Connection = StateConnecting }, cached: true},
		{name: "unreachable", change: func(h *DashboardHostView) { h.Connection = StateUnreachable }, cached: true},
		{name: "authorization refused", change: func(h *DashboardHostView) { h.Connection = StateRefused }, cached: true},
		{name: "fresh name old reply", change: func(h *DashboardHostView) { h.LastReply = now.Add(-31 * time.Second) }, cached: true},
	}
	for _, test := range cases {
		host := fresh
		test.change(&host)
		want := "garden"
		if test.cached {
			want += " · cached name"
		}
		if got := host.NameLabel(now); got != want {
			t.Errorf("%s: label %q, want %q", test.name, got, want)
		}
		if host.Connection == "" || host.Host.MachineName != "garden" {
			t.Errorf("%s: fixture lost connection or owner declaration", test.name)
		}
	}
}
