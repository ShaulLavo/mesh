package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
		final, err := runProgram(ctx, newModel(hostCatalog(catalog), time.Now()), input, output)
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
			}
		}
		hosts[index] = host{alias: catalog.Host.Alias, stale: catalog.Stale, sessions: sessions}
	}
	return hosts
}

func cliSelection(selected selection) cli.PickerSelection {
	switch selected := selected.(type) {
	case cancelSelection:
		return cli.PickerSelection{}
	case attachSelection:
		return cli.PickerSelection{HostAlias: selected.hostAlias, SessionID: selected.sessionID}
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
