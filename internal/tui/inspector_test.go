package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	teatest "github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
	terminalstate "github.com/shaul/mesh/internal/terminal"
)

func TestSessionInspectorStates(t *testing.T) {
	// Activity is relative to the host's observation time. Host and client
	// clocks do not have to agree for "12s ago" to remain true.
	observedAt := pickerTestNow.Add(-5 * time.Hour)
	lastOutput := observedAt.Add(-12 * time.Second)
	preview := make([]string, inspectionPreviewRows)
	for index := range preview {
		preview[index] = fmt.Sprintf("screen row %02d", index+1)
	}
	ready := cli.SessionInspection{
		ObservedAt:        observedAt,
		CurrentDirectory:  "/home/shaul/src/mesh/internal/worker",
		DirectorySource:   cli.SessionDirectoryProcess,
		ForegroundCommand: "go test ./...",
		TerminalTitle:     "mesh tests",
		LastOutputAt:      &lastOutput,
		Attached:          true,
		Preview:           preview,
	}

	for _, test := range []struct {
		name  string
		setup func(model) model
	}{
		{
			name: "ready",
			setup: func(current model) model {
				return deliverCurrentInspection(t, current, ready, nil)
			},
		},
		{
			name:  "loading",
			setup: func(current model) model { return current },
		},
		{
			name: "unavailable",
			setup: func(current model) model {
				current.hosts[current.selectedHost].stale = true
				current.restartInspectionLoop()
				return current
			},
		},
		{
			name: "failed",
			setup: func(current model) model {
				return deliverCurrentInspection(t, current, cli.SessionInspection{}, errors.New("worker did not answer"))
			},
		},
		{
			name: "stale",
			setup: func(current model) model {
				current = deliverCurrentInspection(t, current, ready, nil)
				_ = current.inspectSelected()
				return deliverCurrentInspection(t, current, cli.SessionInspection{}, errors.New("refresh timed out"))
			},
		},
		{
			name: "full preview",
			setup: func(current model) model {
				current = deliverCurrentInspection(t, current, ready, nil)
				return updateModel(t, current, key(tea.KeySpace))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspect := func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
				return ready, nil
			}
			current := newInspectingModel(context.Background(), pickerFixture(), inspect, pickerTestNow)
			current = updateModel(t, current, tea.WindowSizeMsg{Width: 80, Height: 24})
			current = updateModel(t, current, key(tea.KeyEnter))
			current = test.setup(current)
			view := current.View().Content
			assertFits(t, view, 80, 24)

			plain := ansi.Strip(view)
			switch test.name {
			case "ready":
				for _, want := range []string{"pc.tail.example", "worker · go", "current path", "launched", "claude --dangerously-skip-permissions", "foreground", "go test ./...", "mesh tests", "in use", "output 12s ago", "screen row 24", "enter take over"} {
					if !strings.Contains(plain, want) {
						t.Fatalf("ready inspector lacks %q:\n%s", want, plain)
					}
				}
				if strings.Contains(plain, "screen row 14") {
					t.Fatalf("inline preview showed an old row instead of its tail:\n%s", plain)
				}
			case "loading":
				if !strings.Contains(plain, "started in") || !strings.Contains(plain, "Loading current screen") {
					t.Fatalf("loading inspector lacks an honest fallback:\n%s", plain)
				}
			case "unavailable":
				if !strings.Contains(plain, "host is offline") || !strings.Contains(plain, "started in") {
					t.Fatalf("offline inspector lacks its fallback and reason:\n%s", plain)
				}
			case "failed":
				if !strings.Contains(plain, "Inspect failed: worker did not answer") {
					t.Fatalf("failed inspector leaked outside its panel:\n%s", plain)
				}
			case "stale":
				if !strings.Contains(plain, "last live view · refresh failed") || !strings.Contains(plain, "screen row 24") {
					t.Fatalf("stale inspector discarded its last good view:\n%s", plain)
				}
			case "full preview":
				if !strings.Contains(plain, "screen row 24") || strings.Contains(plain, "screen row 01") {
					t.Fatalf("full preview did not retain the newest rows:\n%s", plain)
				}
				if !strings.Contains(plain, "esc details") {
					t.Fatalf("full preview footer does not match Escape behavior:\n%s", plain)
				}
			}
			golden.RequireEqual(t, cleanSnapshot(view))
		})
	}
}

func TestInspectorRequestsOnlyTheSelectedSessionWithinBounds(t *testing.T) {
	var got cli.PickerInspectRequest
	inspect := func(_ context.Context, request cli.PickerInspectRequest) (cli.SessionInspection, error) {
		got = request
		return cli.SessionInspection{ObservedAt: pickerTestNow}, nil
	}
	current := newInspectingModel(context.Background(), pickerFixture(), inspect, pickerTestNow)
	_ = current.showSessions()
	command := current.inspectSelected()
	if command == nil {
		t.Fatal("selected live session produced no inspection command")
	}
	message, ok := command().(inspectionResultMsg)
	if !ok {
		t.Fatalf("inspection command returned %T", command())
	}
	if got != (cli.PickerInspectRequest{HostAlias: "pc", SessionID: "7K3D", PreviewCols: 76, PreviewRows: 24}) {
		t.Fatalf("inspection request = %#v", got)
	}
	if message.target != (inspectionTarget{hostAlias: "pc", sessionID: "7K3D"}) {
		t.Fatalf("inspection target = %#v", message.target)
	}
}

func TestPickerPopulatesLiveLabelsForEveryVisibleSession(t *testing.T) {
	hosts := []host{{
		alias: "pc",
		sessions: []session{
			{id: "7K3D", state: "detached", command: []string{"bash"}, cwd: "/home/shaul", createdAt: pickerTestNow},
			{id: "91AZ", state: "detached", command: []string{"bash"}, cwd: "/home/shaul", createdAt: pickerTestNow},
		},
	}}
	inspect := func(_ context.Context, request cli.PickerInspectRequest) (cli.SessionInspection, error) {
		inspection := cli.SessionInspection{
			ObservedAt:        pickerTestNow,
			DirectorySource:   cli.SessionDirectoryProcess,
			ForegroundCommand: "claude",
		}
		switch request.SessionID {
		case "7K3D":
			inspection.CurrentDirectory = "/work/alpha"
		case "91AZ":
			inspection.CurrentDirectory = "/work/beta"
			inspection.ForegroundCommand = "npm"
		default:
			t.Fatalf("unexpected inspection target %q", request.SessionID)
		}
		return inspection, nil
	}

	program := teatest.NewTestModel(
		t,
		newInspectingModel(context.Background(), hosts, inspect, pickerTestNow),
		teatest.WithInitialTermSize(80, 24),
	)
	program.Send(key(tea.KeyEnter))
	teatest.WaitFor(t, program.Output(), func(output []byte) bool {
		plain := ansi.Strip(string(output))
		return strings.Contains(plain, "alpha · claude") && strings.Contains(plain, "beta · npm")
	}, teatest.WithDuration(500*time.Millisecond), teatest.WithCheckInterval(10*time.Millisecond))
	program.Send(runeKey('q'))
	_ = program.FinalModel(t, teatest.WithFinalTimeout(time.Second))
}

func TestLiveSessionLabelsStayWithTheirSessionsWhenTheCursorMoves(t *testing.T) {
	hosts := []host{{
		alias: "pc",
		sessions: []session{
			{id: "7K3D", state: "detached", command: []string{"bash"}, cwd: "/home/shaul", createdAt: pickerTestNow},
			{id: "91AZ", state: "detached", command: []string{"bash"}, cwd: "/home/shaul", createdAt: pickerTestNow},
		},
	}}
	current := newInspectingModel(context.Background(), hosts, func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return cli.SessionInspection{}, nil
	}, pickerTestNow)
	current = updateModel(t, current, key(tea.KeyEnter))
	current = deliverCurrentInspection(t, current, cli.SessionInspection{
		ObservedAt:        pickerTestNow,
		CurrentDirectory:  "/work/alpha",
		DirectorySource:   cli.SessionDirectoryProcess,
		ForegroundCommand: "claude",
	}, nil)
	current = updateModel(t, current, key(tea.KeyDown))
	current = deliverCurrentInspection(t, current, cli.SessionInspection{
		ObservedAt:        pickerTestNow,
		CurrentDirectory:  "/work/beta",
		DirectorySource:   cli.SessionDirectoryProcess,
		ForegroundCommand: "npm",
	}, nil)

	view := ansi.Strip(current.View().Content)
	for _, want := range []string{"alpha · claude", "beta · npm"} {
		if !strings.Contains(view, want) {
			t.Fatalf("session list lost %q after cursor movement:\n%s", want, view)
		}
	}
}

func TestInspectorIgnoresLateResultsForAnotherRow(t *testing.T) {
	inspect := func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return cli.SessionInspection{}, nil
	}
	current := newInspectingModel(context.Background(), pickerFixture(), inspect, pickerTestNow)
	current = updateModel(t, current, key(tea.KeyEnter))
	oldTarget := current.inspection.target
	oldGeneration := current.inspection.generation
	current = updateModel(t, current, key(tea.KeyDown))
	if current.inspection.target.sessionID != "91AZ" {
		t.Fatalf("selected target = %#v", current.inspection.target)
	}

	current = updateModel(t, current, inspectionResultMsg{
		target:     oldTarget,
		generation: oldGeneration,
		value: cli.SessionInspection{
			ObservedAt:       pickerTestNow,
			CurrentDirectory: "/wrong/session",
			Preview:          []string{"WRONG ROW"},
		},
	})
	if current.inspection.kind != inspectionLoading || strings.Contains(ansi.Strip(current.View().Content), "WRONG ROW") {
		t.Fatalf("late result changed the new selection:\n%s", ansi.Strip(current.View().Content))
	}
}

func TestInspectionTickStartsANewRefreshWithoutClearingTheView(t *testing.T) {
	ready := cli.SessionInspection{ObservedAt: pickerTestNow, Preview: []string{"still visible"}}
	inspect := func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return ready, nil
	}
	current := newInspectingModel(context.Background(), pickerFixture(), inspect, pickerTestNow)
	current = updateModel(t, current, key(tea.KeyEnter))
	current = deliverCurrentInspection(t, current, ready, nil)
	previousGeneration := current.inspection.generation

	updated, command := current.Update(inspectionTickMsg{epoch: current.refreshEpoch, at: pickerTestNow.Add(2 * time.Second)})
	refreshed, ok := updated.(model)
	if !ok {
		t.Fatalf("updated model has type %T", updated)
	}
	if command == nil || refreshed.inspection.generation <= previousGeneration || !refreshed.inspection.refreshing {
		t.Fatalf("refresh state = generation %d, refreshing %t, command nil %t", refreshed.inspection.generation, refreshed.inspection.refreshing, command == nil)
	}
	if view := ansi.Strip(refreshed.View().Content); !strings.Contains(view, "still visible") || !strings.Contains(view, "├─ live ─") || strings.Contains(view, "refreshing") {
		t.Fatalf("background refresh changed the stable live view:\n%s", view)
	}
}

func TestChangingRowsCancelsThePreviousInspection(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	inspect := func(ctx context.Context, _ cli.PickerInspectRequest) (cli.SessionInspection, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return cli.SessionInspection{}, ctx.Err()
	}
	current := newInspectingModel(context.Background(), pickerFixture(), inspect, pickerTestNow)
	_ = current.showSessions()
	command := current.inspectSelected()
	if command == nil {
		t.Fatal("initial selection produced no inspection command")
	}
	go command()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("inspection did not start")
	}

	current = updateModel(t, current, key(tea.KeyDown))
	defer current.stopInspection()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("changing rows did not cancel the previous inspection")
	}
}

func TestInspectionOutputAgeAdvancesFromTheLastObservation(t *testing.T) {
	remoteObservedAt := pickerTestNow.Add(-5 * time.Hour)
	lastOutputAt := remoteObservedAt.Add(-12 * time.Second)
	state := inspectionState{
		value: cli.SessionInspection{
			ObservedAt:   remoteObservedAt,
			LastOutputAt: &lastOutputAt,
		},
		receivedAt: pickerTestNow,
	}
	if got := inspectionOutputAge(pickerTestNow.Add(10*time.Minute), state); got != "output 10m ago" {
		t.Fatalf("advanced output age = %q, want output 10m ago", got)
	}
}

func TestFailedRefreshLabelsExpandedPreviewAsStale(t *testing.T) {
	ready := cli.SessionInspection{ObservedAt: pickerTestNow, Preview: []string{"last good screen"}}
	current := newInspectingModel(context.Background(), pickerFixture(), func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return ready, nil
	}, pickerTestNow)
	current = updateModel(t, current, key(tea.KeyEnter))
	current = deliverCurrentInspection(t, current, ready, nil)
	_ = current.inspectSelected()
	current = deliverCurrentInspection(t, current, cli.SessionInspection{}, errors.New("worker timed out"))
	current = updateModel(t, current, key(tea.KeySpace))
	view := ansi.Strip(current.View().Content)
	for _, want := range []string{"last screen · refresh failed", "worker timed out", "last good screen"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expanded stale preview lacks %q:\n%s", want, view)
		}
	}
}

func TestSessionInspectorFitsSmallTerminal(t *testing.T) {
	lastOutput := pickerTestNow.Add(-3 * time.Second)
	ready := cli.SessionInspection{
		ObservedAt:        pickerTestNow,
		CurrentDirectory:  "/home/shaul/src/mesh/internal/worker",
		ForegroundCommand: "go test ./...",
		TerminalTitle:     "mesh tests",
		LastOutputAt:      &lastOutput,
		Preview:           []string{"one", "two", "three", "four"},
	}
	inspect := func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return ready, nil
	}
	current := newInspectingModel(context.Background(), pickerFixture(), inspect, pickerTestNow)
	current = updateModel(t, current, tea.WindowSizeMsg{Width: 52, Height: 16})
	current = updateModel(t, current, key(tea.KeyEnter))
	current = deliverCurrentInspection(t, current, ready, nil)
	assertFits(t, current.View().Content, 52, 16)
	current = updateModel(t, current, key(tea.KeySpace))
	assertFits(t, current.View().Content, 52, 16)
}

func TestSessionLayoutSpendsSpaceOnPreviewBeforeBlankRows(t *testing.T) {
	for _, test := range []struct {
		name         string
		sessionCount int
		wantList     int
		wantPreview  int
	}{
		{name: "two sessions", sessionCount: 2, wantList: 2, wantPreview: 10},
		{name: "eight sessions", sessionCount: 8, wantList: 8, wantPreview: 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			sessions := make([]session, test.sessionCount)
			for index := range sessions {
				sessions[index] = session{id: fmt.Sprintf("S%03d", index), state: "detached"}
			}
			current := newModel([]host{{alias: "pc", sessions: sessions}}, pickerTestNow)
			_ = current.showSessions()
			listRows, previewRows, showPanel := current.sessionLayout(20)
			if !showPanel || listRows != test.wantList || previewRows != test.wantPreview {
				t.Fatalf("layout = list %d, preview %d, panel %t", listRows, previewRows, showPanel)
			}
		})
	}
}

func TestInspectorEscapesLiveRemoteText(t *testing.T) {
	malicious := "ATTACKER\tFAKE\nROW\x1b[31m\u202e"
	inspect := func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return cli.SessionInspection{}, nil
	}
	current := newInspectingModel(context.Background(), pickerFixture(), inspect, pickerTestNow)
	current = updateModel(t, current, key(tea.KeyEnter))
	current = deliverCurrentInspection(t, current, cli.SessionInspection{
		ObservedAt:        pickerTestNow,
		CurrentDirectory:  "/tmp/" + malicious,
		ForegroundCommand: malicious,
		TerminalTitle:     malicious,
		Preview:           []string{malicious},
	}, nil)
	view := ansi.Strip(current.View().Content)
	if strings.ContainsRune(view, '\u202e') || strings.Contains(view, "\nROW") || strings.ContainsAny(strings.ReplaceAll(view, "\n", ""), "\t\r\x1b") {
		t.Fatalf("unsafe live inspection view = %q", view)
	}
}

func TestInspectorNamesABlankCurrentScreen(t *testing.T) {
	current := newInspectingModel(context.Background(), pickerFixture(), func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return cli.SessionInspection{}, nil
	}, pickerTestNow)
	current = updateModel(t, current, key(tea.KeyEnter))
	current = deliverCurrentInspection(t, current, cli.SessionInspection{
		ObservedAt: pickerTestNow,
		Preview:    []string{"", "   "},
	}, nil)
	if view := ansi.Strip(current.View().Content); !strings.Contains(view, "(screen is blank)") {
		t.Fatalf("blank inspection view =\n%s", view)
	}
}

func TestInspectorStillRendersANonPickerMeshProcess(t *testing.T) {
	current := newInspectingModel(context.Background(), pickerFixture(), func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return cli.SessionInspection{}, nil
	}, pickerTestNow)
	current = updateModel(t, current, key(tea.KeyEnter))
	current = deliverCurrentInspection(t, current, cli.SessionInspection{
		ObservedAt:        pickerTestNow,
		ForegroundCommand: "mesh serve",
		TerminalTitle:     "service logs",
		Preview:           []string{"ordinary mesh output"},
	}, nil)

	view := ansi.Strip(current.View().Content)
	if !strings.Contains(view, "ordinary mesh output") || strings.Contains(view, "preview hidden") {
		t.Fatalf("ordinary mesh process was mistaken for the picker:\n%s", view)
	}
}

func TestContainingSessionWithoutAnInitialSnapshotIsNeverInspectedAfterDrawing(t *testing.T) {
	requests := 0
	current := newInspectingModel(context.Background(), []host{{
		id: "host-a", alias: "pc",
		sessions: []session{{id: "7K3D", state: "detached"}},
	}}, func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		requests++
		return cli.SessionInspection{
			ObservedAt:        pickerTestNow,
			ForegroundCommand: "mesh",
			TerminalTitle:     "Mesh",
			Preview:           []string{"PICKER COPYING ITSELF"},
		}, nil
	}, pickerTestNow)
	current.containingSessions[containingSessionKey{hostID: "host-a", sessionID: "7K3D"}] = containingSessionState{}
	current.enterSessions(0)
	if command := current.inspectSelected(); command != nil {
		t.Fatal("containing session without a pre-picker snapshot started a live inspection")
	}
	if requests != 0 {
		t.Fatalf("containing session made %d live inspections, want none", requests)
	}
	if view := ansi.Strip(current.View().Content); strings.Contains(view, "PICKER COPYING ITSELF") || !strings.Contains(view, "screen before the picker opened could not be captured") {
		t.Fatalf("missing pre-picker snapshot was not reported honestly:\n%s", view)
	}
}

func TestInspectorIdentityDoesNotHideAnotherMeshPicker(t *testing.T) {
	current := newInspectingModel(context.Background(), []host{{
		id: "host-a", alias: "pc",
		sessions: []session{{id: "7K3D", state: "detached"}},
	}}, func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return cli.SessionInspection{}, nil
	}, pickerTestNow)
	current.containingSessions[containingSessionKey{hostID: "host-a", sessionID: "91AZ"}] = containingSessionState{}
	current.enterSessions(0)
	_ = current.inspectSelected()
	current = deliverCurrentInspection(t, current, cli.SessionInspection{
		ObservedAt:        pickerTestNow,
		ForegroundCommand: "mesh",
		TerminalTitle:     "Mesh",
		Preview:           []string{"OTHER PICKER"},
	}, nil)

	if view := ansi.Strip(current.View().Content); !strings.Contains(view, "OTHER PICKER") || strings.Contains(view, "preview hidden") {
		t.Fatalf("identity guard hid a different picker:\n%s", view)
	}
}

func TestContainingSessionUsesScreenCapturedBeforePickerStarted(t *testing.T) {
	inspectCalls := 0
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{
			Host: cli.HostRecord{ID: "host-a", Alias: "pc"},
			Sessions: []protocol.SessionInfo{{
				ID: "7K3D", HostID: "host-a", State: "running", CreatedAt: pickerTestNow,
			}},
		}},
		Inspect: func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
			inspectCalls++
			return cli.SessionInspection{
				ObservedAt: pickerTestNow,
				Preview:    []string{"PICKER COPYING ITSELF"},
			}, nil
		},
		ContainingSessions: []cli.PickerContainingSession{{
			Identity: protocol.SessionIdentity{HostID: "host-a", SessionID: "7K3D"},
			Snapshot: &cli.SessionInspection{
				ObservedAt:        pickerTestNow.Add(-time.Second),
				ForegroundCommand: "claude",
				TerminalTitle:     "Claude",
				Attached:          true,
				Preview:           []string{"CLAUDE SCREEN BEFORE PICKER"},
			},
			ReceivedAt: pickerTestNow,
		}},
	}, pickerTestNow)
	current.enterSessions(0)

	if command := current.inspectSelected(); command != nil {
		t.Fatal("containing session started a live screen inspection after the picker was visible")
	}
	if inspectCalls != 0 {
		t.Fatalf("containing session made %d live inspection calls, want none", inspectCalls)
	}
	view := ansi.Strip(current.View().Content)
	if !strings.Contains(view, "CLAUDE SCREEN BEFORE PICKER") || strings.Contains(view, "PICKER COPYING ITSELF") || strings.Contains(view, "preview hidden") {
		t.Fatalf("containing session did not render its pre-picker screen:\n%s", view)
	}
	if !strings.Contains(view, "├─ before picker ─") {
		t.Fatalf("pre-picker screen was not labeled honestly:\n%s", view)
	}

	current = updateModel(t, current, key(tea.KeySpace))
	if !current.fullPreview {
		t.Fatal("space did not open the captured screen")
	}
	if view := ansi.Strip(current.View().Content); !strings.Contains(view, "CLAUDE SCREEN BEFORE PICKER") {
		t.Fatalf("full preview lost the pre-picker screen:\n%s", view)
	}
}

func TestEveryNestedContainingSessionUsesItsSnapshotWhileUnrelatedSessionsStayLive(t *testing.T) {
	const unrelatedID = "5J2P"
	chainIDs := []string{"7K3D", "91AZ", "Q8ME"}
	snapshotText := []string{"INNER BEFORE PICKER", "MIDDLE BEFORE PICKER", "OUTER BEFORE PICKER"}
	sessions := make([]protocol.SessionInfo, 0, len(chainIDs)+1)
	containing := make([]cli.PickerContainingSession, len(chainIDs))
	for index, sessionID := range chainIDs {
		sessions = append(sessions, protocol.SessionInfo{
			ID: sessionID, HostID: "host-a", State: "detached", CreatedAt: pickerTestNow,
		})
		containing[index] = cli.PickerContainingSession{
			Identity: protocol.SessionIdentity{HostID: "host-a", SessionID: sessionID},
			Snapshot: &cli.SessionInspection{
				ObservedAt: pickerTestNow,
				Preview:    []string{snapshotText[index]},
			},
			ReceivedAt: pickerTestNow,
		}
	}
	sessions = append(sessions, protocol.SessionInfo{
		ID: unrelatedID, HostID: "host-a", State: "detached", CreatedAt: pickerTestNow,
	})
	requests := make(chan cli.PickerInspectRequest, len(sessions))
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{
			Host:     cli.HostRecord{ID: "host-a", Alias: "pc"},
			Sessions: sessions,
		}},
		Inspect: func(_ context.Context, request cli.PickerInspectRequest) (cli.SessionInspection, error) {
			requests <- request
			return cli.SessionInspection{
				ObservedAt: pickerTestNow,
				Preview:    []string{"LIVE " + request.SessionID},
			}, nil
		},
		ContainingSessions: containing,
	}, pickerTestNow)
	current.enterSessions(0)

	for index, sessionID := range chainIDs {
		current.list.Select(index)
		if command := current.inspectSelected(); command != nil {
			t.Fatalf("containing session %s started a live inspection", sessionID)
		}
		if len(requests) != 0 {
			t.Fatalf("containing session %s reached the live inspector", sessionID)
		}
		view := ansi.Strip(current.View().Content)
		if !strings.Contains(view, snapshotText[index]) || strings.Contains(view, "LIVE "+sessionID) {
			t.Fatalf("containing session %s did not show its own pre-picker snapshot:\n%s", sessionID, view)
		}
	}

	current.list.Select(len(chainIDs))
	command := current.inspectSelected()
	if command == nil {
		t.Fatal("unrelated session did not start a live inspection")
	}
	message, ok := command().(inspectionResultMsg)
	if !ok {
		t.Fatalf("live inspection command returned %T", command())
	}
	request := <-requests
	if request.HostAlias != "pc" || request.SessionID != unrelatedID {
		t.Fatalf("live inspection request = %#v", request)
	}
	current = current.applyInspection(message)
	if view := ansi.Strip(current.View().Content); !strings.Contains(view, "LIVE "+unrelatedID) {
		t.Fatalf("unrelated session did not render its live screen:\n%s", view)
	}
}

func TestContainingSessionSnapshotOutputAgeKeepsAdvancing(t *testing.T) {
	lastOutputAt := pickerTestNow
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{
			Host: cli.HostRecord{ID: "host-a", Alias: "pc"},
			Sessions: []protocol.SessionInfo{{
				ID: "7K3D", HostID: "host-a", State: "running", CreatedAt: pickerTestNow,
			}},
		}},
		ContainingSessions: []cli.PickerContainingSession{{
			Identity: protocol.SessionIdentity{HostID: "host-a", SessionID: "7K3D"},
			Snapshot: &cli.SessionInspection{
				ObservedAt:   pickerTestNow,
				LastOutputAt: &lastOutputAt,
				Preview:      []string{"parent screen"},
			},
			ReceivedAt: pickerTestNow,
		}},
	}, pickerTestNow)
	current.enterSessions(0)
	_ = current.inspectSelected()

	current.now = pickerTestNow.Add(10 * time.Second)
	_ = current.inspectSelected()
	if got := current.detailsFor(current.currentHost().sessions[0]).output; got != "output 10s ago" {
		t.Fatalf("pre-picker output age after refresh = %q, want %q", got, "output 10s ago")
	}
}

func TestContainingSessionUsesLiveCatalogAttachmentState(t *testing.T) {
	current := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{
			Host: cli.HostRecord{ID: "host-a", Alias: "pc"},
			Sessions: []protocol.SessionInfo{{
				ID: "7K3D", HostID: "host-a", State: "running", CreatedAt: pickerTestNow,
			}},
		}},
		ContainingSessions: []cli.PickerContainingSession{{
			Identity: protocol.SessionIdentity{HostID: "host-a", SessionID: "7K3D"},
			Snapshot: &cli.SessionInspection{
				ObservedAt: pickerTestNow,
				Attached:   true,
				Preview:    []string{"parent screen"},
			},
			ReceivedAt: pickerTestNow,
		}},
	}, pickerTestNow)
	current.enterSessions(0)
	_ = current.inspectSelected()

	current.hosts[0].sessions[0].state = "detached"
	if err := current.list.SetItems(sessionItems(current.hosts[0].sessions)); err != nil {
		t.Fatal(err)
	}
	current.refreshSessionDelegate()
	details := current.detailsFor(current.hosts[0].sessions[0])
	if details.attachment != "detached" {
		t.Fatalf("pre-picker attachment state = %q, want live catalog state detached", details.attachment)
	}
	view := ansi.Strip(current.View().Content)
	if !strings.Contains(view, "detached") || strings.Contains(view, "in use") {
		t.Fatalf("pre-picker row ignored the live detached state:\n%s", view)
	}
}

func TestInspectorRendersStructuredTerminalStylesWithoutBleedingIntoTheBox(t *testing.T) {
	current := newModel(pickerFixture(), pickerTestNow)
	inspection := cli.SessionInspection{
		Preview: []string{"DBIR"},
		StyledPreview: []protocol.PreviewLine{{Runs: []protocol.PreviewRun{
			{Text: "D"},
			{
				Text: "B",
				Style: protocol.PreviewStyle{
					Foreground:     protocol.PreviewColor{Kind: protocol.PreviewColorBasic, Value: 1},
					Background:     protocol.PreviewColor{Kind: protocol.PreviewColorRGB, Value: 0x010203},
					UnderlineColor: protocol.PreviewColor{Kind: protocol.PreviewColorIndexed, Value: 45},
					Bold:           true,
					Faint:          true,
					Italic:         true,
					Reverse:        true,
					Strikethrough:  true,
					Underline:      protocol.PreviewUnderlineCurly,
				},
			},
			{Text: "I", Style: protocol.PreviewStyle{Foreground: protocol.PreviewColor{Kind: protocol.PreviewColorIndexed, Value: 46}}},
			{Text: "R", Style: protocol.PreviewStyle{Foreground: protocol.PreviewColor{Kind: protocol.PreviewColorRGB, Value: 0x040506}}},
		}}},
	}

	rendered, styled := current.renderInspectionPreview(inspection)
	if !styled || len(rendered) != 1 || ansi.Strip(rendered[0]) != "DBIR" {
		t.Fatalf("rendered preview = %#v, styled %t", rendered, styled)
	}
	screen := terminalstate.NewScreen(12, 1)
	if _, err := screen.Write([]byte(current.boxRow(rendered[0], 12))); err != nil {
		t.Fatal(err)
	}
	preview := screen.Preview(12, 1)
	if len(preview.StyledLines) != 1 {
		t.Fatalf("parsed preview = %#v", preview)
	}
	var defaultRun, highlighted, indexed, rgb, border *terminalstate.PreviewRun
	for index := range preview.StyledLines[0].Runs {
		run := &preview.StyledLines[0].Runs[index]
		if strings.Contains(run.Text, "D") {
			defaultRun = run
		}
		if run.Text == "B" {
			highlighted = run
		}
		if run.Text == "I" {
			indexed = run
		}
		if run.Text == "R" {
			rgb = run
		}
		if strings.HasSuffix(run.Text, "│") {
			border = run
		}
	}
	if defaultRun == nil || defaultRun.Style != (terminalstate.PreviewStyle{}) {
		t.Fatalf("default preview run = %#v", defaultRun)
	}
	if highlighted == nil || highlighted.Style != (terminalstate.PreviewStyle{
		Foreground:     terminalstate.PreviewColor{Kind: terminalstate.PreviewColorBasic, Value: 1},
		Background:     terminalstate.PreviewColor{Kind: terminalstate.PreviewColorRGB, Value: 0x010203},
		UnderlineColor: terminalstate.PreviewColor{Kind: terminalstate.PreviewColorIndexed, Value: 45},
		Bold:           true,
		Faint:          true,
		Italic:         true,
		Reverse:        true,
		Strikethrough:  true,
		Underline:      terminalstate.PreviewUnderlineCurly,
	}) {
		t.Fatalf("highlighted preview run = %#v", highlighted)
	}
	if indexed == nil || indexed.Style.Foreground != (terminalstate.PreviewColor{Kind: terminalstate.PreviewColorIndexed, Value: 46}) {
		t.Fatalf("indexed preview run = %#v", indexed)
	}
	if rgb == nil || rgb.Style.Foreground != (terminalstate.PreviewColor{Kind: terminalstate.PreviewColorRGB, Value: 0x040506}) {
		t.Fatalf("RGB preview run = %#v", rgb)
	}
	if border == nil || border.Style.Background.Kind != terminalstate.PreviewColorDefault || border.Style.Bold || border.Style.Italic {
		t.Fatalf("preview style leaked into box border: %#v", border)
	}
}

func TestInspectorPreservesValidatedStructuredPreviewTextExactly(t *testing.T) {
	current := newModel(pickerFixture(), pickerTestNow)
	plain := `C:\src\mesh "quoted"`
	inspection := cli.SessionInspection{
		Preview:       []string{plain},
		StyledPreview: []protocol.PreviewLine{{Runs: []protocol.PreviewRun{{Text: plain}}}},
	}

	rendered, styled := current.renderInspectionPreview(inspection)
	if !styled || len(rendered) != 1 || ansi.Strip(rendered[0]) != plain {
		t.Fatalf("rendered structured preview = %#v, want exact text %q", rendered, plain)
	}
}

func TestInspectorPlainPreviewFallbackEscapesTerminalControls(t *testing.T) {
	current := newModel(pickerFixture(), pickerTestNow)
	unsafe := "safe\x1b[31munsafe\u202ereversed"
	rendered, styled := current.renderInspectionPreview(cli.SessionInspection{Preview: []string{unsafe}})
	if styled || len(rendered) != 1 {
		t.Fatalf("rendered plain fallback = %#v, styled %t", rendered, styled)
	}
	if strings.ContainsAny(rendered[0], "\x1b") || strings.ContainsRune(rendered[0], '\u202e') {
		t.Fatalf("plain fallback exposed terminal controls: %q", rendered[0])
	}
	if !strings.Contains(rendered[0], `\x1b`) || !strings.Contains(rendered[0], `\u202e`) {
		t.Fatalf("plain fallback did not visibly escape controls: %q", rendered[0])
	}
}

func TestInspectorPlainPreviewFallbackPreservesEmojiJoiners(t *testing.T) {
	const emoji = "🤸‍♂️✨"
	current := newModel(pickerFixture(), pickerTestNow)
	rendered, styled := current.renderInspectionPreview(cli.SessionInspection{Preview: []string{emoji}})
	if styled || len(rendered) != 1 || ansi.Strip(rendered[0]) != emoji {
		t.Fatalf("rendered plain fallback = %#v, styled %t, want %q", rendered, styled, emoji)
	}
}

func TestHostCatalogUsesCleanRoute(t *testing.T) {
	hosts := hostCatalog(cli.PickerInput{Hosts: []cli.HostSessions{
		{Host: cli.HostRecord{Alias: "pc", TailscaleName: "pc.tail.example", Endpoint: "ws://100.64.0.2:7337/mesh"}},
		{Host: cli.HostRecord{Alias: "pi", Endpoint: "ws://100.64.0.8:7447/mesh"}},
	}})
	if hosts[0].route != "pc.tail.example" || hosts[1].route != "100.64.0.8:7447" {
		t.Fatalf("routes = %q, %q", hosts[0].route, hosts[1].route)
	}
}

func TestOfflineExpandedPreviewStillWarnsBeforeAttach(t *testing.T) {
	current := newModel([]host{{
		alias:    "pc",
		stale:    true,
		sessions: []session{{id: "7K3D", state: "running"}},
	}}, pickerTestNow)
	current = updateModel(t, current, key(tea.KeyEnter))
	current = updateModel(t, current, key(tea.KeySpace))
	if view := ansi.Strip(current.View().Content); !strings.Contains(view, "enter try attach") {
		t.Fatalf("offline expanded footer =\n%s", view)
	}
}

func TestEmptySessionHasAnHonestLabel(t *testing.T) {
	if got := sessionLabel(session{}); got != "untitled session" {
		t.Fatalf("empty session label = %q", got)
	}
}

func deliverCurrentInspection(t *testing.T, current model, value cli.SessionInspection, err error) model {
	t.Helper()
	if current.inspection.target == (inspectionTarget{}) {
		t.Fatal("model has no current inspection target")
	}
	return updateModel(t, current, inspectionResultMsg{
		target:     current.inspection.target,
		generation: current.inspection.generation,
		value:      value,
		err:        err,
	})
}
