package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
)

func TestPickerRefreshAddsSessionsWithoutMovingTheSelection(t *testing.T) {
	initial := protocol.SessionInfo{
		ID: "7K3D", State: "detached", Command: []string{"bash"}, Cwd: "/work/old", CreatedAt: pickerTestNow.Add(-time.Minute),
	}
	added := protocol.SessionInfo{
		ID: "91AZ", State: "detached", Command: []string{"codex"}, Cwd: "/work/new", CreatedAt: pickerTestNow,
	}
	refreshCalls := 0
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{
			Host:     cli.HostRecord{Alias: "pc"},
			Sessions: []protocol.SessionInfo{initial},
		}},
		Refresh: func(_ context.Context, alias string) (cli.PickerHostSnapshot, error) {
			refreshCalls++
			if alias != "pc" {
				t.Fatalf("refresh alias = %q, want pc", alias)
			}
			sessions := []protocol.SessionInfo{initial}
			if refreshCalls > 1 {
				sessions = []protocol.SessionInfo{added, initial}
			}
			return cli.PickerHostSnapshot{Sessions: cli.HostSessions{
				Host:     cli.HostRecord{Alias: "pc"},
				Sessions: sessions,
			}}, nil
		},
	}, pickerTestNow)
	if refreshCalls != 0 {
		t.Fatalf("refresh calls before opening host = %d, want 0", refreshCalls)
	}
	openCommand := current.showSessions()
	firstResult := catalogRefreshMessage(t, openCommand)
	updated, nextRefresh := current.Update(firstResult)
	current = updated.(model)
	if refreshCalls != 1 || nextRefresh == nil {
		t.Fatalf("initial refresh calls = %d, next command nil %t", refreshCalls, nextRefresh == nil)
	}
	if got := current.selectedSessionID(); got != "7K3D" {
		t.Fatalf("initial selected session = %q, want 7K3D", got)
	}

	updated, command := current.Update(catalogRefreshTickMsg{
		epoch: current.catalogEpoch,
		at:    pickerTestNow.Add(2 * time.Second),
	})
	refreshing := updated.(model)
	if command == nil {
		t.Fatal("catalog refresh tick produced no refresh command")
	}
	updated, _ = refreshing.Update(command())
	refreshed := updated.(model)

	if refreshCalls != 2 {
		t.Fatalf("refresh calls = %d, want 2", refreshCalls)
	}
	if refreshed.screen != sessionScreen || refreshed.currentHost().alias != "pc" || refreshed.selection != nil {
		t.Fatalf("refresh changed picker navigation: screen %d, host %q, selection %#v", refreshed.screen, refreshed.currentHost().alias, refreshed.selection)
	}
	if got := refreshed.selectedSessionID(); got != "7K3D" {
		t.Fatalf("selected session after prepend = %q, want 7K3D", got)
	}
	if len(refreshed.currentHost().sessions) != 2 || len(refreshed.list.Items()) != 2 {
		t.Fatalf("refreshed session counts = host %d, list %d; want 2", len(refreshed.currentHost().sessions), len(refreshed.list.Items()))
	}
	if view := ansi.Strip(refreshed.View().Content); !strings.Contains(view, "91AZ") {
		t.Fatalf("refreshed picker does not show 91AZ:\n%s", view)
	}
}

func TestPickerRefreshKeepsANewlyListedContainingSessionOnItsPrePickerSnapshot(t *testing.T) {
	host := cli.HostRecord{ID: "host-a", Alias: "pc"}
	inner := protocol.SessionInfo{ID: "7K3D", HostID: host.ID, State: "detached", CreatedAt: pickerTestNow}
	outer := protocol.SessionInfo{ID: "91AZ", HostID: host.ID, State: "detached", CreatedAt: pickerTestNow}
	unrelated := protocol.SessionInfo{ID: "Q8ME", HostID: host.ID, State: "detached", CreatedAt: pickerTestNow}
	inspectCalls := 0
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{Host: host, Sessions: []protocol.SessionInfo{inner, unrelated}}},
		Inspect: func(_ context.Context, request cli.PickerInspectRequest) (cli.SessionInspection, error) {
			inspectCalls++
			return cli.SessionInspection{
				ObservedAt: pickerTestNow,
				Preview:    []string{"LIVE " + request.SessionID},
			}, nil
		},
		ContainingSessions: []cli.PickerContainingSession{
			{
				Identity:   protocol.SessionIdentity{HostID: host.ID, SessionID: inner.ID},
				Snapshot:   &cli.SessionInspection{ObservedAt: pickerTestNow, Preview: []string{"INNER BEFORE PICKER"}},
				ReceivedAt: pickerTestNow,
			},
			{
				Identity:   protocol.SessionIdentity{HostID: host.ID, SessionID: outer.ID},
				Snapshot:   &cli.SessionInspection{ObservedAt: pickerTestNow, Preview: []string{"OUTER BEFORE PICKER"}},
				ReceivedAt: pickerTestNow,
			},
		},
	}, pickerTestNow)
	current.enterSessions(0)
	if command := current.inspectSelected(); command != nil {
		t.Fatal("initial containing session started a live inspection")
	}

	refreshed, _ := current.applyCatalogRefresh(catalogRefreshResultMsg{
		epoch:     current.catalogEpoch,
		hostAlias: host.Alias,
		snapshot: cli.PickerHostSnapshot{Sessions: cli.HostSessions{
			Host: host, Sessions: []protocol.SessionInfo{inner, outer, unrelated},
		}},
	})
	current = refreshed
	current.list.Select(1)
	if got := current.selectedSessionID(); got != outer.ID {
		t.Fatalf("selected refreshed session = %q, want %s", got, outer.ID)
	}
	if command := current.inspectSelected(); command != nil {
		t.Fatal("newly listed containing session started a live inspection")
	}
	if inspectCalls != 0 {
		t.Fatalf("containing chain made %d live inspection calls, want none", inspectCalls)
	}
	view := ansi.Strip(current.View().Content)
	if !strings.Contains(view, "OUTER BEFORE PICKER") || strings.Contains(view, "LIVE "+outer.ID) {
		t.Fatalf("newly listed containing session lost its pre-picker snapshot:\n%s", view)
	}
}

func TestPickerRefreshErrorRetainsCatalogAndKeepsPolling(t *testing.T) {
	host := cli.HostRecord{Alias: "pc", ID: "host-id"}
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{Host: host, Sessions: []protocol.SessionInfo{{
			ID: "7K3D", HostID: host.ID, State: "detached", CreatedAt: pickerTestNow,
		}}}},
		Refresh: func(context.Context, string) (cli.PickerHostSnapshot, error) {
			return cli.PickerHostSnapshot{}, errors.New("temporary failure")
		},
	}, pickerTestNow)
	command := current.showSessions()
	updated, nextRefresh := current.Update(catalogRefreshMessage(t, command))
	refreshed := updated.(model)
	if got := refreshed.selectedSessionID(); got != "7K3D" {
		t.Fatalf("selected session after refresh error = %q, want 7K3D", got)
	}
	if nextRefresh == nil {
		t.Fatal("refresh error stopped catalog polling")
	}
}

func TestPickerRefreshTracksDetachAndExitWithoutLeavingThePanel(t *testing.T) {
	host := cli.HostRecord{Alias: "pc", ID: "host-id"}
	refreshCalls := 0
	snapshot := func(state string) cli.PickerHostSnapshot {
		return cli.PickerHostSnapshot{
			Sessions: cli.HostSessions{Host: host, Sessions: []protocol.SessionInfo{{
				ID: "7K3D", HostID: host.ID, State: state, CreatedAt: pickerTestNow,
			}}},
			Services: &cli.PickerServiceCatalog{},
		}
	}
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{snapshot("running").Sessions},
		Refresh: func(context.Context, string) (cli.PickerHostSnapshot, error) {
			refreshCalls++
			if refreshCalls == 1 {
				return snapshot("detached"), nil
			}
			return snapshot("exited"), nil
		},
	}, pickerTestNow)
	updated, _ := current.Update(catalogRefreshMessage(t, current.showSessions()))
	current = updated.(model)
	if state, _ := sessionState(current.currentHost().sessions, "7K3D"); state != "detached" || current.screen != sessionScreen || current.selectedSessionID() != "7K3D" {
		t.Fatalf("detached refresh state %q, screen %d, selected %q", state, current.screen, current.selectedSessionID())
	}
	updated, command := current.Update(catalogRefreshTickMsg{epoch: current.catalogEpoch, at: pickerTestNow.Add(2 * time.Second)})
	updated, _ = updated.(model).Update(command())
	current = updated.(model)
	if state, _ := sessionState(current.currentHost().sessions, "7K3D"); state != "exited" || current.screen != sessionScreen || current.selectedSessionID() != "7K3D" || current.inspection.kind != inspectionUnavailable {
		t.Fatalf("exit refresh state %q, screen %d, selected %q, inspection %d", state, current.screen, current.selectedSessionID(), current.inspection.kind)
	}
}

func catalogRefreshMessage(t *testing.T, command tea.Cmd) catalogRefreshResultMsg {
	t.Helper()
	commands := []tea.Cmd{command}
	for len(commands) > 0 {
		current := commands[0]
		commands = commands[1:]
		if current == nil {
			continue
		}
		switch message := current().(type) {
		case catalogRefreshResultMsg:
			return message
		case tea.BatchMsg:
			commands = append(commands, message...)
		}
	}
	t.Fatal("command tree contained no catalog refresh result")
	return catalogRefreshResultMsg{}
}
