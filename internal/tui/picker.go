package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"

	"github.com/shaul/mesh/internal/cli"
)

// ErrNonInteractive tells callers that bare mesh needs a real terminal.
var ErrNonInteractive = errors.New("interactive picker needs a terminal; run mesh <host> or mesh <session-id>")

// NewCLIPicker adapts the terminal picker to the selection-only CLI boundary.
func NewCLIPicker(input, output *os.File) cli.PickerFunc {
	return func(ctx context.Context, catalog cli.PickerInput) (cli.PickerSelection, error) {
		if input == nil || output == nil || !term.IsTerminal(input.Fd()) || !term.IsTerminal(output.Fd()) {
			return cli.PickerSelection{}, ErrNonInteractive
		}
		pickerContext, cancel := context.WithCancel(ctx)
		defer cancel()
		final, err := runProgram(pickerContext, newPickerModel(pickerContext, catalog, time.Now()), input, output)
		if err != nil {
			return cli.PickerSelection{}, fmt.Errorf("interactive picker: %w", err)
		}
		finished, ok := final.(model)
		if !ok {
			return cli.PickerSelection{}, fmt.Errorf("interactive picker returned model %T", final)
		}
		if finished.selection == nil {
			finished.selection = cancelSelection{}
		}
		return cliSelection(finished.selection), nil
	}
}

func newPickerModel(ctx context.Context, catalog cli.PickerInput, now time.Time) model {
	current := newInspectingModel(ctx, hostCatalog(catalog), catalog.Inspect, now)
	current.refresh = catalog.Refresh
	current.act = catalog.Action
	current.loadHosts = catalog.LoadHosts
	for _, containing := range catalog.ContainingSessions {
		current.containingPath = append(current.containingPath, containing.Identity)
		state := containingSessionState{receivedAt: containing.ReceivedAt}
		if containing.Snapshot != nil {
			value := cloneSessionInspection(*containing.Snapshot)
			state.snapshot = &value
			if state.receivedAt.IsZero() {
				state.receivedAt = now
			}
		}
		current.containingSessions[containingSessionKey{
			hostID: containing.Identity.HostID, sessionID: containing.Identity.SessionID,
		}] = state
	}
	current.rememberContainingSessionSnapshots()
	if catalog.OpenHostAlias != "" {
		current.openHostSessions(catalog.OpenHostAlias)
	}
	return current
}

func (m *model) rememberContainingSessionSnapshots() {
	for _, current := range m.hosts {
		for key, containing := range m.containingSessions {
			if current.id != key.hostID || containing.snapshot == nil {
				continue
			}
			m.rememberSessionSummary(
				inspectionTarget{hostAlias: current.alias, sessionID: key.sessionID},
				*containing.snapshot,
			)
		}
	}
}

func runProgram(ctx context.Context, initial tea.Model, input io.Reader, output io.Writer) (tea.Model, error) {
	program := tea.NewProgram(
		initial,
		tea.WithContext(ctx),
		tea.WithInput(input),
		tea.WithOutput(output),
		tea.WithoutSignals(),
	)
	return program.Run()
}

func hostCatalog(input cli.PickerInput) []host {
	hosts := make([]host, len(input.Hosts))
	for index, catalog := range input.Hosts {
		sessions := make([]session, len(catalog.Sessions))
		for sessionIndex, current := range catalog.Sessions {
			sessions[sessionIndex] = session{
				id:        current.ID,
				state:     current.State,
				command:   append([]string(nil), current.Command...),
				cwd:       current.Cwd,
				createdAt: current.CreatedAt,
				recovery:  cloneRecovery(current.Recovery), recoveryError: current.RecoveryError,
				replacementID: current.ReplacementID, recoveredFrom: current.RecoveredFrom,
			}
		}
		route := catalog.Host.TailscaleName
		if route == "" {
			route = endpointRoute(catalog.Host.Endpoint)
		}
		if catalog.Local {
			route = "this host"
		}
		hosts[index] = host{id: catalog.Host.ID, alias: catalog.Host.Alias, route: route, stale: catalog.Stale, local: catalog.Local, sessions: groupRecoveryAttempts(sessions)}
	}
	sort.SliceStable(hosts, func(i, j int) bool { return hosts[i].local && !hosts[j].local })
	return hosts
}

func endpointRoute(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return endpoint
}

func cliSelection(selected selection) cli.PickerSelection {
	switch selected := selected.(type) {
	case cancelSelection:
		return cli.PickerSelection{}
	case attachSelection:
		return cli.PickerSelection{HostAlias: selected.hostAlias, SessionID: selected.sessionID, Relaunch: selected.relaunch, TakeOver: selected.takeOver, RecoveryAction: selected.recoveryAction}
	case newSelection:
		return cli.PickerSelection{HostAlias: selected.hostAlias, New: true}
	case resumeSelection:
		return cli.PickerSelection{HostAlias: selected.hostAlias}
	case wakeSelection:
		return cli.PickerSelection{HostAlias: selected.hostAlias, Wake: true}
	default:
		panic(fmt.Sprintf("tui: unknown picker selection %T", selected))
	}
}
