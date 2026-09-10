package tui

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
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

func TestPickerRefreshReordersWithoutChangingSelectedSession(t *testing.T) {
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
	assertSessionOrder(t, current, "OLD", "ADDED", "RECENT")
	if current.selectedSessionID() != "OLD" || current.list.Index() != 0 {
		t.Fatal("refresh did not follow the selected session to its new position")
	}
	if state, _ := sessionState(current.currentHost().sessions, "OLD"); state != "running" {
		t.Fatal("stable ordering prevented a state refresh")
	}
	current.showHosts()
	current.enterSessions(0)
	assertSessionOrder(t, current, "OLD", "ADDED", "RECENT")
}

func TestPickerRecencyOrdersPreviousRecoveryAttemptsByTheirOwnUpdate(t *testing.T) {
	attached := pickerTestNow
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{Host: cli.HostRecord{Alias: "pc"}, Sessions: []protocol.SessionInfo{
			{ID: "OTHER", State: "detached", CreatedAt: pickerTestNow.Add(-time.Minute)},
			{ID: "OLD", State: "interrupted", ReplacementID: "REPLACEMENT", CreatedAt: pickerTestNow.Add(-time.Hour)},
			{ID: "REPLACEMENT", State: "detached", CreatedAt: pickerTestNow.Add(-time.Minute), LastAttachedAt: &attached},
		}}}, OpenHostAlias: "pc",
	}, pickerTestNow)
	assertSessionOrder(t, current, "REPLACEMENT", "OTHER", "OLD")
	if !current.currentHost().sessions[2].previousAttempt {
		t.Fatal("sorting lost the previous attempt label")
	}
}

func TestPickerOrdersByCheckpointAcrossStatesAndRefreshes(t *testing.T) {
	attached := pickerTestNow.Add(-time.Hour)
	rows := []protocol.SessionInfo{
		{ID: "NEW", State: "detached", CreatedAt: pickerTestNow.Add(-time.Minute)},
		{ID: "OLD", State: "interrupted", CreatedAt: pickerTestNow.Add(-24 * time.Hour), LastAttachedAt: &attached,
			Recovery: &recovery.Record{CheckpointAt: pickerTestNow}},
		{ID: "EXITED", State: "exited", CreatedAt: pickerTestNow.Add(-2 * time.Hour),
			Recovery: &recovery.Record{CheckpointAt: pickerTestNow.Add(-30 * time.Minute)}},
	}
	host := cli.HostRecord{Alias: "pc"}
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{Host: host, Sessions: rows}}, OpenHostAlias: host.Alias,
	}, pickerTestNow)
	assertSessionOrder(t, current, "OLD", "NEW", "EXITED")
	rows[2].Recovery.CheckpointAt = pickerTestNow.Add(time.Minute)
	current, _ = current.applyCatalogRefresh(catalogRefreshResultMsg{
		epoch: current.catalogEpoch, hostAlias: host.Alias,
		snapshot: cli.PickerHostSnapshot{Sessions: cli.HostSessions{Host: host, Sessions: rows}},
	})
	assertSessionOrder(t, current, "EXITED", "OLD", "NEW")
	if current.selectedSessionID() != "OLD" {
		t.Fatal("checkpoint refresh changed the selected session")
	}
}

func TestWindowPickerOrdersByUpdatesBeforeState(t *testing.T) {
	input := windowFixture()
	input.Sessions[0].Recovery = &recovery.Record{CheckpointAt: pickerTestNow.Add(2 * time.Minute)}
	input.Sessions[1].Recovery = &recovery.Record{CheckpointAt: pickerTestNow.Add(time.Minute)}
	current := newWindowModel(context.Background(), input, pickerTestNow)
	assertSessionOrder(t, current.picker, "91AZ", "Q8ME", "BC45", "7K3D")
	if !current.selected || current.picker.selectedSessionID() != "Q8ME" {
		t.Fatal("compact prompt did not preselect the most recently updated available session")
	}
}

func TestWindowPickerPrefersRecentlyUsedDetachedSession(t *testing.T) {
	input := windowFixture()
	attached := pickerTestNow.Add(time.Minute)
	input.Sessions[2].LastAttachedAt = &attached
	current := newWindowModel(context.Background(), input, pickerTestNow)
	assertSessionOrder(t, current.picker, "7K3D", "91AZ", "BC45", "Q8ME")
}
