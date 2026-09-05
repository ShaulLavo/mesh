package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
)

func TestPickerSessionActionsStayInTheOpenPanel(t *testing.T) {
	host := cli.HostRecord{Alias: "pc", ID: "host-id"}
	for _, test := range []struct {
		name         string
		key          rune
		action       cli.PickerSessionAction
		targetState  string
		wantSelected string
	}{
		{
			name: "kill updates the selected row", key: 'k', action: cli.PickerKillSession, targetState: "detached",
			wantSelected: "7K3D",
		},
		{
			name: "remove selects the neighboring row", key: 'x', action: cli.PickerRemoveSession, targetState: "exited",
			wantSelected: "0007",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			actionCalls := 0
			refreshCalls := 0
			var request cli.PickerSessionActionRequest
			rows := make([]protocol.SessionInfo, 10)
			for index := range rows {
				rows[index] = protocol.SessionInfo{
					ID: fmt.Sprintf("%04d", index), HostID: host.ID, State: "detached", CreatedAt: pickerTestNow,
				}
			}
			rows[6].ID = "7K3D"
			rows[6].State = test.targetState
			refreshedRows := append([]protocol.SessionInfo(nil), rows...)
			if test.action == cli.PickerKillSession {
				refreshedRows[6].State = "exited"
			} else {
				refreshedRows = append(refreshedRows[:6], refreshedRows[7:]...)
			}
			initial := cli.HostSessions{Host: host, Sessions: rows}
			current := newPickerModel(context.Background(), cli.PickerInput{
				Hosts: []cli.HostSessions{initial},
				Action: func(_ context.Context, got cli.PickerSessionActionRequest) error {
					actionCalls++
					request = got
					return nil
				},
				Refresh: func(context.Context, string) (cli.PickerHostSnapshot, error) {
					refreshCalls++
					return cli.PickerHostSnapshot{
						Sessions: cli.HostSessions{Host: host, Sessions: refreshedRows},
						Services: &cli.PickerServiceCatalog{},
					}, nil
				},
			}, pickerTestNow)
			current.width = 80
			current.height = 16
			_ = current.showSessions()
			current.list.Select(6)
			startPage, startCursor := current.list.Paginator.Page, current.list.Cursor()
			if startPage == 0 {
				t.Fatal("fixture did not put the selected row on a later page")
			}
			preActionEpoch := current.catalogEpoch

			updated, actionCommand := current.Update(runeKey(test.key))
			acting := updated.(model)
			if acting.screen != sessionScreen || acting.selection != nil || actionCommand == nil {
				t.Fatalf("action left panel: screen %d, selection %#v, command nil %t", acting.screen, acting.selection, actionCommand == nil)
			}
			if actionCalls != 0 {
				t.Fatalf("action ran synchronously during key update: %d calls", actionCalls)
			}
			if acting.list.Paginator.Page != startPage || acting.list.Cursor() != startCursor || acting.selectedSessionID() != "7K3D" {
				t.Fatalf("pending action moved list: page %d cursor %d selected %q", acting.list.Paginator.Page, acting.list.Cursor(), acting.selectedSessionID())
			}
			updated, refreshCommand := acting.Update(actionCommand())
			acted := updated.(model)
			if actionCalls != 1 || refreshCommand == nil || acted.screen != sessionScreen || acted.selection != nil {
				t.Fatalf("action calls %d, refresh nil %t, screen %d, selection %#v", actionCalls, refreshCommand == nil, acted.screen, acted.selection)
			}
			if acted.catalogEpoch <= preActionEpoch || acted.list.Paginator.Page != startPage || acted.list.Cursor() != startCursor {
				t.Fatalf("action result epoch %d, page %d, cursor %d; started %d/%d/%d", acted.catalogEpoch, acted.list.Paginator.Page, acted.list.Cursor(), preActionEpoch, startPage, startCursor)
			}
			updated, repeatedCommand := acted.Update(runeKey(test.key))
			acted = updated.(model)
			if repeatedCommand != nil || actionCalls != 1 {
				t.Fatalf("repeated action during reconciliation returned command nil %t after %d calls", repeatedCommand == nil, actionCalls)
			}
			if request != (cli.PickerSessionActionRequest{HostAlias: "pc", SessionID: "7K3D", Action: test.action}) {
				t.Fatalf("action request = %#v", request)
			}
			updated, _ = acted.Update(catalogRefreshMessage(t, refreshCommand))
			refreshed := updated.(model)
			if refreshCalls != 1 || refreshed.screen != sessionScreen || refreshed.selection != nil {
				t.Fatalf("refresh calls %d, screen %d, selection %#v", refreshCalls, refreshed.screen, refreshed.selection)
			}
			if got := refreshed.selectedSessionID(); got != test.wantSelected {
				t.Fatalf("selected session = %q, want %q", got, test.wantSelected)
			}
			if test.action == cli.PickerKillSession {
				if state, exists := sessionState(refreshed.currentHost().sessions, "7K3D"); !exists || state != "exited" {
					t.Fatalf("killed session state = %q, exists %t", state, exists)
				}
			} else if _, exists := sessionState(refreshed.currentHost().sessions, "7K3D"); exists || len(refreshed.currentHost().sessions) != 9 {
				t.Fatalf("removed session remains in %#v", refreshed.currentHost().sessions)
			}
			if refreshed.list.Paginator.Page != startPage || refreshed.list.Cursor() != startCursor {
				t.Fatalf("refreshed list moved to page %d cursor %d, want %d/%d", refreshed.list.Paginator.Page, refreshed.list.Cursor(), startPage, startCursor)
			}
			late := catalogRefreshResultMsg{
				epoch: preActionEpoch, hostAlias: "pc",
				snapshot: cli.PickerHostSnapshot{Sessions: initial, Services: &cli.PickerServiceCatalog{}},
			}
			updated, command := refreshed.Update(late)
			if command != nil || updated.(model).selectedSessionID() != test.wantSelected {
				t.Fatalf("pre-action refresh changed post-action panel: command nil %t, selected %q", command == nil, updated.(model).selectedSessionID())
			}
		})
	}
}

func TestRemovingTheOnlySessionLeavesAnEmptyLivePanel(t *testing.T) {
	host := cli.HostRecord{Alias: "pc", ID: "host-id"}
	initial := cli.HostSessions{Host: host, Sessions: []protocol.SessionInfo{{
		ID: "7K3D", HostID: host.ID, State: "exited", CreatedAt: pickerTestNow,
	}}}
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts:  []cli.HostSessions{initial},
		Action: func(context.Context, cli.PickerSessionActionRequest) error { return nil },
		Refresh: func(context.Context, string) (cli.PickerHostSnapshot, error) {
			return cli.PickerHostSnapshot{
				Sessions: cli.HostSessions{Host: host}, Services: &cli.PickerServiceCatalog{},
			}, nil
		},
	}, pickerTestNow)
	_ = current.showSessions()
	updated, actionCommand := current.Update(runeKey('x'))
	updated, refreshCommand := updated.(model).Update(actionCommand())
	updated, _ = updated.(model).Update(catalogRefreshMessage(t, refreshCommand))
	empty := updated.(model)
	if empty.screen != sessionScreen || empty.selectedSessionID() != "" || len(empty.currentHost().sessions) != 0 || empty.inspection.kind != inspectionUnavailable {
		t.Fatalf("empty panel screen %d, selected %q, sessions %d, inspection %d", empty.screen, empty.selectedSessionID(), len(empty.currentHost().sessions), empty.inspection.kind)
	}
	if view := empty.View().Content; !strings.Contains(view, "No sessions") || !strings.Contains(view, "0 served") {
		t.Fatalf("empty panel lacks live empty state:\n%s", view)
	}
}

func TestLateSessionActionResultCannotFinishANewerActionForTheSameTarget(t *testing.T) {
	host := cli.HostRecord{Alias: "pc", ID: "host-id"}
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{Host: host, Sessions: []protocol.SessionInfo{{
			ID: "7K3D", HostID: host.ID, State: "detached", CreatedAt: pickerTestNow,
		}}}},
		Action: func(context.Context, cli.PickerSessionActionRequest) error { return nil },
		Refresh: func(context.Context, string) (cli.PickerHostSnapshot, error) {
			return cli.PickerHostSnapshot{}, nil
		},
	}, pickerTestNow)
	_ = current.showSessions()
	updated, firstCommand := current.Update(runeKey('k'))
	first := updated.(model)
	firstGeneration := first.sessionAction.generation

	updated, _ = first.Update(key(tea.KeyEscape))
	reopened := updated.(model)
	updated, _ = reopened.Update(key(tea.KeyEnter))
	reopened = updated.(model)
	updated, secondCommand := reopened.Update(runeKey('k'))
	second := updated.(model)
	if secondCommand == nil || second.sessionAction.generation == firstGeneration {
		t.Fatalf("new action generation = %d, old = %d", second.sessionAction.generation, firstGeneration)
	}

	updated, refreshCommand := second.Update(firstCommand())
	afterLate := updated.(model)
	if refreshCommand != nil || afterLate.sessionAction.phase != sessionActionRunning || afterLate.sessionAction.generation != second.sessionAction.generation {
		t.Fatalf("late result changed newer action: command nil %t, phase %d, generation %d", refreshCommand == nil, afterLate.sessionAction.phase, afterLate.sessionAction.generation)
	}
	afterLate.invalidateSessionAction()
}

func TestLeavingSessionPanelCancelsAnInFlightActionAndIgnoresItsResult(t *testing.T) {
	host := cli.HostRecord{Alias: "pc", ID: "host-id"}
	started := make(chan struct{})
	canceled := make(chan struct{})
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{Host: host, Sessions: []protocol.SessionInfo{{
			ID: "7K3D", HostID: host.ID, State: "detached", CreatedAt: pickerTestNow,
		}}}},
		Action: func(ctx context.Context, _ cli.PickerSessionActionRequest) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			return ctx.Err()
		},
	}, pickerTestNow)
	_ = current.showSessions()
	updated, actionCommand := current.Update(runeKey('k'))
	acting := updated.(model)
	results := make(chan tea.Msg, 1)
	go func() { results <- actionCommand() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("session action did not start")
	}

	updated, _ = acting.Update(key(tea.KeyEscape))
	escaped := updated.(model)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("leaving the panel did not cancel its session action")
	}
	updated, command := escaped.Update(<-results)
	afterLate := updated.(model)
	if command != nil || afterLate.screen != hostScreen || afterLate.sessionAction.phase != sessionActionIdle {
		t.Fatalf("late canceled result changed host panel: command nil %t, screen %d, phase %d", command == nil, afterLate.screen, afterLate.sessionAction.phase)
	}
}

func TestPickerSessionActionFailureStaysVisibleAndKeepsRefreshing(t *testing.T) {
	host := cli.HostRecord{Alias: "pc", ID: "host-id"}
	sessions := cli.HostSessions{Host: host, Sessions: []protocol.SessionInfo{{
		ID: "7K3D", HostID: host.ID, State: "detached", CreatedAt: pickerTestNow,
	}}}
	refreshCalls := 0
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{sessions},
		Action: func(context.Context, cli.PickerSessionActionRequest) error {
			return errors.New("host refused the kill")
		},
		Refresh: func(context.Context, string) (cli.PickerHostSnapshot, error) {
			refreshCalls++
			changed := sessions
			changed.Sessions = append(append([]protocol.SessionInfo(nil), sessions.Sessions...), protocol.SessionInfo{
				ID: "91AZ", HostID: host.ID, State: "detached", CreatedAt: pickerTestNow,
			})
			return cli.PickerHostSnapshot{Sessions: changed, Services: &cli.PickerServiceCatalog{}}, nil
		},
	}, pickerTestNow)
	_ = current.showSessions()
	updated, actionCommand := current.Update(runeKey('k'))
	updated, refreshCommand := updated.(model).Update(actionCommand())
	failed := updated.(model)

	if failed.screen != sessionScreen || failed.selection != nil || refreshCommand == nil || !strings.Contains(failed.notice, "host refused the kill") {
		t.Fatalf("failed action screen %d, selection %#v, refresh nil %t, notice %q", failed.screen, failed.selection, refreshCommand == nil, failed.notice)
	}
	updated, repeatedCommand := failed.Update(runeKey('k'))
	failed = updated.(model)
	if repeatedCommand != nil || !strings.Contains(failed.notice, "host refused the kill") {
		t.Fatalf("repeat during failed-action reconciliation returned command nil %t, notice %q", repeatedCommand == nil, failed.notice)
	}
	updated, _ = failed.Update(catalogRefreshMessage(t, refreshCommand))
	failed = updated.(model)
	if refreshCalls != 1 || failed.selectedSessionID() != "7K3D" || !strings.Contains(failed.View().Content, "host refused the kill") {
		t.Fatalf("failed action refresh calls %d, selected %q, notice %q", refreshCalls, failed.selectedSessionID(), failed.notice)
	}
}
