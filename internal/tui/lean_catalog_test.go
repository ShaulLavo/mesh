package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
)

func TestLeanCatalogPickerRequestsSelectedSavedPreviewAndRendersIdentically(t *testing.T) {
	full := savedPickerSession()
	before := recoveryPicker(full)
	lean := full
	protocol.LeanRecoveryInfo(&lean, *full.Recovery)
	after := recoveryPicker(lean)
	calls := 0
	after.inspect = func(_ context.Context, request cli.PickerInspectRequest) (cli.SessionInspection, error) {
		calls++
		if request.SessionID != full.ID || request.HostAlias != "local" {
			t.Fatalf("inspected unselected row: %+v", request)
		}
		return cli.SessionInspection{Recovery: full.Recovery}, nil
	}
	command := after.inspectSelected()
	if command == nil {
		t.Fatal("lean ended row never fetched its preview")
	}
	if after.inspection.kind != inspectionLoading {
		t.Fatal("lean preview used an empty saved output state while loading")
	}
	message, ok := command().(inspectionResultMsg)
	if !ok {
		t.Fatal("saved inspection returned an unexpected message")
	}
	after = after.applyInspection(message)
	if calls != 1 {
		t.Fatalf("inspection calls = %d, want 1", calls)
	}
	if before.View().Content != after.View().Content {
		t.Fatalf("picker changed after lean preview hydration\nbefore:\n%s\nafter:\n%s", before.View().Content, after.View().Content)
	}
	before = updateModel(t, before, runeKey(' '))
	after = updateModel(t, after, runeKey(' '))
	if before.View().Content != after.View().Content {
		t.Fatal("expanded saved preview changed")
	}
	t.Logf("Verified identical selected and expanded saved previews:\n%s", after.View().Content)
}

func TestOldDaemonFullCatalogNeedsNoSavedInspection(t *testing.T) {
	current := recoveryPicker(savedPickerSession())
	current.inspect = func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		t.Fatal("old daemon full preview triggered a new saved inspection")
		return cli.SessionInspection{}, nil
	}
	if current.inspectSelected() != nil {
		t.Fatal("legacy catalog requested saved inspection")
	}
}

func TestSavedPreviewHydrationMatchesHostAndSession(t *testing.T) {
	full := savedPickerSession()
	lean := full
	protocol.LeanRecoveryInfo(&lean, *full.Recovery)
	current := recoveryPicker(lean)
	_, selected, ok := current.currentSession()
	if !ok {
		t.Fatal("missing selected row")
	}
	current.inspection = inspectionState{kind: inspectionReady, target: inspectionTarget{hostAlias: "another-host", sessionID: full.ID}, hasValue: true, value: cli.SessionInspection{Recovery: full.Recovery}}
	if details := current.savedDetailsFor(selected); len(details.preview) != 1 || details.preview[0] != "Loading saved preview…" {
		t.Fatalf("another host's same-ID preview was displayed: %+v", details)
	}
}

func TestLeanCatalogLiveToEndedSavedFailureShowsError(t *testing.T) {
	for _, state := range []string{"interrupted", "exited"} {
		t.Run(state, func(t *testing.T) {
			row := savedPickerSession()
			protocol.LeanRecoveryInfo(&row, *row.Recovery)
			row.State = "running"
			current := recoveryPicker(row)
			current.inspect = func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
				return cli.SessionInspection{Preview: []string{"previous live screen"}}, nil
			}
			command := current.inspectSelected()
			current = current.applyInspection(command().(inspectionResultMsg))
			if !current.inspection.hasValue || current.inspection.value.Recovery != nil {
				t.Fatal("live inspection prerequisite failed")
			}
			row.State = state
			current.inspect = func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
				return cli.SessionInspection{}, errors.New("saved checkpoint request failed")
			}
			current, _ = current.applyCatalogRefresh(catalogRefreshResultMsg{
				epoch: current.catalogEpoch, hostAlias: "local",
				snapshot: cli.PickerHostSnapshot{Sessions: cli.HostSessions{
					Host: cli.HostRecord{Alias: "local"}, Local: true, Sessions: []protocol.SessionInfo{row},
				}},
			})
			command = current.inspectSelected()
			if command == nil {
				t.Fatal("ended lean selection never requested saved inspection")
			}
			current = current.applyInspection(command().(inspectionResultMsg))
			view := ansi.Strip(current.View().Content)
			t.Logf("live-to-ended view after settled error:\n%s", view)
			if !strings.Contains(view, "saved checkpoint request failed") || strings.Contains(view, "Loading saved preview") {
				t.Fatalf("settled saved-inspection error is hidden; kind=%v hasValue=%v recovery=%v problem=%q", current.inspection.kind, current.inspection.hasValue, current.inspection.value.Recovery, current.inspection.problem)
			}
		})
	}
}

func TestLeanCatalogInitialSavedFailureShowsError(t *testing.T) {
	row := savedPickerSession()
	protocol.LeanRecoveryInfo(&row, *row.Recovery)
	current := recoveryPicker(row)
	current.inspect = func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return cli.SessionInspection{}, errors.New("saved checkpoint request failed")
	}
	command := current.inspectSelected()
	current = current.applyInspection(command().(inspectionResultMsg))
	view := ansi.Strip(current.View().Content)
	t.Logf("initial saved failure:\n%s", view)
	if !strings.Contains(view, "saved checkpoint request failed") || strings.Contains(view, "Loading saved preview") {
		t.Fatalf("initial saved failure did not display error:\n%s", view)
	}
}

func TestLeanCatalogSavedRefreshFailureRetainsSavedPreview(t *testing.T) {
	full := savedPickerSession()
	row := full
	protocol.LeanRecoveryInfo(&row, *row.Recovery)
	current := recoveryPicker(row)
	current.inspect = func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return cli.SessionInspection{Recovery: full.Recovery}, nil
	}
	command := current.inspectSelected()
	current = current.applyInspection(command().(inspectionResultMsg))
	before := ansi.Strip(current.View().Content)
	current.inspect = func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) {
		return cli.SessionInspection{}, errors.New("saved checkpoint request failed")
	}
	command = current.inspectSelected()
	current = current.applyInspection(command().(inspectionResultMsg))
	view := ansi.Strip(current.View().Content)
	t.Logf("retained saved preview after failed refresh:\n%s", view)
	if !strings.Contains(view, "tests completed") || strings.Contains(view, "Loading saved preview") || view != before {
		t.Fatalf("saved preview changed after failed refresh:\nbefore:\n%s\nafter:\n%s", before, view)
	}
}
