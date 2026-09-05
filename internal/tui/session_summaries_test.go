package tui

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
)

func TestSessionSummaryRefreshIsBoundedToVisibleLiveRows(t *testing.T) {
	sessions := make([]session, 10)
	for index := range sessions {
		sessions[index] = session{id: fmt.Sprintf("S%03d", index), state: "detached"}
	}
	sessions[2].state = "exited"
	requests := make(chan cli.PickerInspectRequest, len(sessions))
	current := newInspectingModel(context.Background(), []host{{alias: "pc", sessions: sessions}}, func(_ context.Context, request cli.PickerInspectRequest) (cli.SessionInspection, error) {
		requests <- request
		return cli.SessionInspection{ObservedAt: pickerTestNow}, nil
	}, pickerTestNow)
	current.enterSessions(0)

	command := current.inspectVisibleSessionSummaries()
	if command == nil {
		t.Fatal("visible live sessions produced no summary command")
	}
	message, ok := command().(sessionSummariesResultMsg)
	if !ok {
		t.Fatalf("summary command returned %T", command())
	}
	got := make([]string, 0, len(requests))
	for len(requests) > 0 {
		request := <-requests
		if request.PreviewCols != 1 || request.PreviewRows != 1 || request.HostAlias != "pc" {
			t.Fatalf("summary request = %#v", request)
		}
		got = append(got, request.SessionID)
	}
	slices.Sort(got)
	want := []string{"S001", "S003", "S004", "S005", "S006", "S007"}
	if !slices.Equal(got, want) {
		t.Fatalf("visible summary targets = %v, want %v", got, want)
	}
	if len(message.values) != len(want) {
		t.Fatalf("summary values = %d, want %d", len(message.values), len(want))
	}
}

func TestSessionSummaryRefreshSkipsEveryNestedContainingSession(t *testing.T) {
	requests := make(chan cli.PickerInspectRequest, 8)
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{
			Host: cli.HostRecord{ID: "host-a", Alias: "pc"},
			Sessions: []protocol.SessionInfo{
				{ID: "5J2P", HostID: "host-a", State: "detached", CreatedAt: pickerTestNow},
				{ID: "7K3D", HostID: "host-a", State: "detached", CreatedAt: pickerTestNow},
				{ID: "91AZ", HostID: "host-a", State: "detached", CreatedAt: pickerTestNow},
				{ID: "Q8ME", HostID: "host-a", State: "detached", CreatedAt: pickerTestNow},
				{ID: "S000", HostID: "host-a", State: "detached", CreatedAt: pickerTestNow},
			},
		}},
		Inspect: func(_ context.Context, request cli.PickerInspectRequest) (cli.SessionInspection, error) {
			requests <- request
			return cli.SessionInspection{ObservedAt: pickerTestNow}, nil
		},
		ContainingSessions: []cli.PickerContainingSession{
			{
				Identity:   protocol.SessionIdentity{HostID: "host-a", SessionID: "7K3D"},
				Snapshot:   &cli.SessionInspection{ObservedAt: pickerTestNow, Preview: []string{"inner"}},
				ReceivedAt: pickerTestNow,
			},
			{
				Identity:   protocol.SessionIdentity{HostID: "host-a", SessionID: "91AZ"},
				Snapshot:   &cli.SessionInspection{ObservedAt: pickerTestNow, Preview: []string{"middle"}},
				ReceivedAt: pickerTestNow,
			},
			{
				Identity:   protocol.SessionIdentity{HostID: "host-a", SessionID: "Q8ME"},
				Snapshot:   &cli.SessionInspection{ObservedAt: pickerTestNow, Preview: []string{"outer"}},
				ReceivedAt: pickerTestNow,
			},
		},
	}, pickerTestNow)
	current.enterSessions(0)
	current.now = pickerTestNow.Add(sessionSummaryRefreshInterval)

	command := current.inspectVisibleSessionSummaries()
	if command == nil {
		t.Fatal("unrelated visible session produced no summary inspection")
	}
	message, ok := command().(sessionSummariesResultMsg)
	if !ok {
		t.Fatalf("summary command returned an unexpected message")
	}
	got := make([]string, 0, len(requests))
	for len(requests) > 0 {
		request := <-requests
		if request.PreviewCols != 1 || request.PreviewRows != 1 {
			t.Fatalf("summary request = %#v", request)
		}
		got = append(got, request.SessionID)
	}
	if want := []string{"S000"}; !slices.Equal(got, want) {
		t.Fatalf("live summary targets = %v, want only unrelated session %v", got, want)
	}
	if len(message.values) != 1 {
		t.Fatalf("summary values = %d, want 1", len(message.values))
	}
}

func TestSessionSummaryFailuresKeepLastGoodLabelsAndCatalogRemovalPrunesThem(t *testing.T) {
	target := inspectionTarget{hostAlias: "pc", sessionID: "7K3D"}
	current := newModel([]host{{alias: "pc", sessions: []session{{id: target.sessionID, state: "detached"}}}}, pickerTestNow)
	current.enterSessions(0)
	current.rememberSessionSummary(target, cli.SessionInspection{
		CurrentDirectory:  "/work/mesh",
		ForegroundCommand: "claude",
	})
	current.summarySeq = 4
	current = current.applySessionSummaries(sessionSummariesResultMsg{
		hostAlias: "pc", generation: current.summarySeq,
	})
	if got := current.summaries[target]; got.currentDirectory != "/work/mesh" || got.foregroundCommand != "claude" {
		t.Fatalf("failed refresh discarded last good summary: %#v", got)
	}

	current.pruneSessionSummaries("pc", nil)
	if _, exists := current.summaries[target]; exists {
		t.Fatal("removed session retained a live summary")
	}
}

func TestSessionSummaryPollingContinuesWhenTheSelectedSessionHasEnded(t *testing.T) {
	current := newInspectingModel(context.Background(), []host{{
		alias: "pc",
		sessions: []session{
			{id: "DONE", state: "exited"},
			{id: "LIVE", state: "detached"},
		},
	}}, func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return cli.SessionInspection{ObservedAt: pickerTestNow}, nil
	}, pickerTestNow)
	current.enterSessions(0)

	command := current.restartInspectionLoop()
	if command == nil {
		t.Fatal("an ended selection stopped polling another visible live session")
	}
	batch, ok := command().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("inspection restart returned %T with %d commands, want summary plus next tick", command(), len(batch))
	}
}

func TestSessionSummaryPollingKeepsATimerWhenOtherRowsAreFresh(t *testing.T) {
	target := inspectionTarget{hostAlias: "pc", sessionID: "LIVE"}
	current := newInspectingModel(context.Background(), []host{{
		alias: "pc",
		sessions: []session{
			{id: "DONE", state: "exited"},
			{id: target.sessionID, state: "detached"},
		},
	}}, func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return cli.SessionInspection{}, nil
	}, pickerTestNow)
	current.enterSessions(0)
	current.rememberSessionSummary(target, cli.SessionInspection{ForegroundCommand: "claude"})

	if command := current.restartInspectionLoop(); command == nil {
		t.Fatal("an ended selection with a fresh neighboring label stopped the refresh timer")
	}

	updated, command := current.Update(inspectionTickMsg{
		epoch: current.refreshEpoch,
		at:    pickerTestNow.Add(sessionSummaryRefreshInterval),
	})
	refreshed := updated.(model)
	defer refreshed.stopSessionSummaryInspection()
	if command == nil || refreshed.cancelSummary == nil {
		t.Fatal("expired neighboring label did not start from the retained refresh timer")
	}
}

func TestSessionSummaryPollingKeepsATimerWhileABatchIsInFlight(t *testing.T) {
	current := newInspectingModel(context.Background(), []host{{
		alias: "pc",
		sessions: []session{
			{id: "DONE", state: "exited"},
			{id: "LIVE", state: "detached"},
		},
	}}, func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return cli.SessionInspection{}, nil
	}, pickerTestNow)
	current.enterSessions(0)
	current.cancelSummary = func() {}
	defer current.stopSessionSummaryInspection()

	if command := current.restartInspectionLoop(); command == nil {
		t.Fatal("an in-flight label batch stopped the refresh timer")
	}
}

func TestSessionSummaryRefreshReusesFreshObservations(t *testing.T) {
	current := newInspectingModel(context.Background(), []host{{
		alias: "pc",
		sessions: []session{
			{id: "SELECTED", state: "detached"},
			{id: "VISIBLE", state: "detached"},
		},
	}}, func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return cli.SessionInspection{CurrentDirectory: "/work/mesh", ForegroundCommand: "claude"}, nil
	}, pickerTestNow)
	current.enterSessions(0)

	message := current.inspectVisibleSessionSummaries()()
	current = current.applySessionSummaries(message.(sessionSummariesResultMsg))
	if command := current.inspectVisibleSessionSummaries(); command != nil {
		t.Fatal("fresh row summaries triggered an immediate duplicate refresh")
	}

	current.now = pickerTestNow.Add(10 * time.Second)
	if command := current.inspectVisibleSessionSummaries(); command == nil {
		t.Fatal("an old row summary never became eligible for refresh")
	}
}

func TestSessionSummaryRefreshCapsConcurrentInspections(t *testing.T) {
	sessions := make([]session, 8)
	for index := range sessions {
		sessions[index] = session{id: fmt.Sprintf("S%03d", index), state: "detached"}
	}

	release := make(chan struct{})
	started := make(chan struct{}, len(sessions))
	var active atomic.Int32
	var maximum atomic.Int32
	current := newInspectingModel(context.Background(), []host{{alias: "pc", sessions: sessions}}, func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		count := active.Add(1)
		for previous := maximum.Load(); count > previous && !maximum.CompareAndSwap(previous, count); previous = maximum.Load() {
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		return cli.SessionInspection{}, nil
	}, pickerTestNow)
	current.enterSessions(0)
	command := current.inspectVisibleSessionSummaries()
	if command == nil {
		t.Fatal("visible sessions produced no summary command")
	}

	done := make(chan tea.Msg, 1)
	go func() { done <- command() }()
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("summary refresh did not fill its concurrency allowance")
		}
	}
	select {
	case <-started:
		t.Fatal("summary refresh started more than three inspections concurrently")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("summary refresh did not finish after inspections were released")
	}
	if got := maximum.Load(); got != 3 {
		t.Fatalf("maximum concurrent inspections = %d, want 3", got)
	}
}
