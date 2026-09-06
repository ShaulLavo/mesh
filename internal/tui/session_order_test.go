package tui

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
)

func assertSessionOrder(t *testing.T, current model, want ...string) {
	t.Helper()
	var got []string
	for _, item := range current.list.Items() {
		got = append(got, item.(sessionItem).session.id)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("picker order = %v, want %v", got, want)
	}
}

func TestPickerOrdersByLastAttachmentWithCreationFallback(t *testing.T) {
	attached := pickerTestNow
	rows := []protocol.SessionInfo{
		{ID: "OLD", CreatedAt: pickerTestNow.Add(-time.Hour)},
		{ID: "NEW", CreatedAt: pickerTestNow.Add(-time.Minute)},
		{ID: "USED", CreatedAt: pickerTestNow.Add(-24 * time.Hour), LastAttachedAt: &attached},
	}
	for _, stale := range []bool{false, true} {
		current := newPickerModel(context.Background(), cli.PickerInput{
			Hosts:         []cli.HostSessions{{Host: cli.HostRecord{Alias: "pc"}, Sessions: rows, Stale: stale}},
			OpenHostAlias: "pc",
		}, pickerTestNow)
		assertSessionOrder(t, current, "USED", "NEW", "OLD")
		if current.selectedSessionID() != "USED" || rows[0].ID != "OLD" {
			t.Fatal("recency did not select the latest row or mutated the source catalog")
		}
	}
}

func TestPickerRefreshFreezesOrderUntilHostReopens(t *testing.T) {
	old := protocol.SessionInfo{ID: "OLD", State: "detached", CreatedAt: pickerTestNow.Add(-time.Hour)}
	recent := protocol.SessionInfo{ID: "RECENT", State: "detached", CreatedAt: pickerTestNow}
	host := cli.HostRecord{Alias: "pc"}
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{Host: host, Sessions: []protocol.SessionInfo{old, recent}}}, OpenHostAlias: "pc",
	}, pickerTestNow)
	current.list.Select(1)
	attached := pickerTestNow.Add(time.Hour)
	old.LastAttachedAt, old.State = &attached, "running"
	added := protocol.SessionInfo{ID: "ADDED", State: "detached", CreatedAt: attached.Add(-time.Minute)}
	current, _ = current.applyCatalogRefresh(catalogRefreshResultMsg{
		epoch: current.catalogEpoch, hostAlias: host.Alias,
		snapshot: cli.PickerHostSnapshot{Sessions: cli.HostSessions{Host: host, Sessions: []protocol.SessionInfo{added, old, recent}}},
	})
	assertSessionOrder(t, current, "RECENT", "OLD", "ADDED")
	if current.selectedSessionID() != "OLD" || current.list.Index() != 1 {
		t.Fatal("refresh moved the selected row")
	}
	if state, _ := sessionState(current.currentHost().sessions, "OLD"); state != "running" {
		t.Fatal("stable ordering prevented a state refresh")
	}
	current.showHosts()
	current.enterSessions(0)
	assertSessionOrder(t, current, "OLD", "ADDED", "RECENT")
}

func TestPickerRecencyKeepsPreviousRecoveryAttemptsTogether(t *testing.T) {
	attached := pickerTestNow
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{Host: cli.HostRecord{Alias: "pc"}, Sessions: []protocol.SessionInfo{
			{ID: "OTHER", State: "detached", CreatedAt: pickerTestNow.Add(-time.Minute)},
			{ID: "OLD", State: "interrupted", ReplacementID: "REPLACEMENT", CreatedAt: pickerTestNow.Add(-time.Hour)},
			{ID: "REPLACEMENT", State: "detached", CreatedAt: pickerTestNow.Add(-time.Minute), LastAttachedAt: &attached},
		}}}, OpenHostAlias: "pc",
	}, pickerTestNow)
	assertSessionOrder(t, current, "REPLACEMENT", "OLD", "OTHER")
}

func TestWindowPickerPrefersRecentlyUsedDetachedSession(t *testing.T) {
	input := windowFixture()
	attached := pickerTestNow.Add(time.Minute)
	input.Sessions[2].LastAttachedAt = &attached
	current := newWindowModel(context.Background(), input, pickerTestNow)
	assertSessionOrder(t, current.picker, "7K3D", "BC45", "Q8ME", "91AZ")
}
