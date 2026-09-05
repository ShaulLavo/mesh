package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

func NewCLIWindowPicker(input, output *os.File) cli.WindowPickerFunc {
	return func(ctx context.Context, catalog cli.WindowInput) (cli.WindowSelection, error) {
		if input == nil || output == nil || !term.IsTerminal(input.Fd()) || !term.IsTerminal(output.Fd()) {
			return cli.WindowSelection{}, ErrNonInteractive
		}
		pickerContext, cancel := context.WithCancel(ctx)
		defer cancel()
		final, err := runProgram(pickerContext, newWindowModel(pickerContext, catalog, time.Now()), input, output)
		if err != nil {
			return cli.WindowSelection{}, fmt.Errorf("window picker: %w", err)
		}
		finished, ok := final.(windowModel)
		if !ok {
			return cli.WindowSelection{}, fmt.Errorf("window picker returned model %T", final)
		}
		if finished.selection == nil {
			return cli.WindowSelection{}, nil
		}
		return *finished.selection, nil
	}
}

type windowModel struct {
	picker    model
	selection *cli.WindowSelection
	selected  bool
	hostNames map[string]string
}

func newWindowModel(ctx context.Context, input cli.WindowInput, now time.Time) windowModel {
	rows := make([]protocol.SessionInfo, 0, len(input.Sessions))
	known := make(map[string]bool, len(input.Sessions))
	for _, current := range input.Sessions {
		known[current.ID] = true
	}
	for _, current := range input.Sessions {
		if current.ReplacementID != "" && known[current.ReplacementID] && current.State == "interrupted" {
			continue
		}
		if current.State == "detached" || current.State == "interrupted" || current.State == "running" {
			rows = append(rows, current)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		left, right := windowSessionOrder(rows[i].State), windowSessionOrder(rows[j].State)
		if left != right {
			return left < right
		}
		return rows[i].CreatedAt.After(rows[j].CreatedAt)
	})
	picker := newPickerModel(ctx, cli.PickerInput{
		Hosts:   []cli.HostSessions{{Host: cli.HostRecord{ID: input.HostID, Alias: input.HostAlias}, Sessions: rows, Local: true}},
		Inspect: input.Inspect, Action: input.Action, OpenHostAlias: input.HostAlias,
	}, now)
	// Empty aliases are valid in isolated callers, but the compact view always
	// opens this machine directly.
	if picker.screen != sessionScreen {
		picker.enterSessions(0)
		picker.refreshOnInit = true
	}
	current := windowModel{picker: picker, selected: len(rows) > 0 && rows[0].State != "running", hostNames: input.HostAliases}
	current.resize()
	return current
}

func windowSessionOrder(state string) int {
	switch state {
	case "detached":
		return 0
	case "interrupted":
		return 1
	default:
		return 2
	}
}

func (m windowModel) Init() tea.Cmd { return m.picker.Init() }

func (m windowModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := message.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "ctrl+c", "esc", "q":
			m.selection = &cli.WindowSelection{}
		case "n":
			m.selection = &cli.WindowSelection{New: true}
		case "l":
			m.selection = &cli.WindowSelection{FullPicker: true}
		case "enter":
			m.choose()
		case "s":
			_, current, ok := m.picker.currentSession()
			if m.selected && ok && endedSession(current) && !sessionActionBusy(m.picker.sessionAction) {
				m.selection = &cli.WindowSelection{SessionID: current.id, Relaunch: true, RecoveryAction: recovery.ActionShell}
			}
		case "x":
			if m.selected && !sessionActionBusy(m.picker.sessionAction) {
				_, current, ok := m.picker.currentSession()
				if ok && current.state == "interrupted" {
					return m, m.picker.startSessionAction(cli.PickerRemoveSession)
				}
			}
			return m, nil
		case "up", "down", "j", "k", "home", "end", "pgup", "pgdown":
			if key.String() == "j" {
				message = tea.KeyPressMsg{Code: tea.KeyDown}
			} else if key.String() == "k" {
				message = tea.KeyPressMsg{Code: tea.KeyUp}
			}
			if !m.selected && len(m.picker.currentHost().sessions) > 0 {
				m.selected = true
				command := m.picker.restartInspectionLoop()
				m.resize()
				return m, command
			}
			updated, command := m.picker.Update(message)
			m.picker = updated.(model)
			m.resize()
			return m, command
		default:
			if digit := key.String(); len(digit) == 1 && digit[0] >= '1' && digit[0] <= '9' {
				index := int(digit[0] - '1')
				if index < len(m.picker.currentHost().sessions) {
					m.picker.list.Select(index)
					m.selected = true
					m.choose()
				}
			}
		}
		if m.selection != nil {
			m.picker.invalidateSessionScreenLoops()
			return m, tea.Quit
		}
		return m, nil
	}
	if result, ok := message.(sessionActionResultMsg); ok {
		return m.applyForget(result)
	}
	updated, command := m.picker.Update(message)
	m.picker = updated.(model)
	m.resize()
	return m, command
}

func (m *windowModel) choose() {
	if sessionActionBusy(m.picker.sessionAction) {
		return
	}
	if !m.selected {
		m.selection = &cli.WindowSelection{New: true}
		return
	}
	_, current, ok := m.picker.currentSession()
	if !ok {
		m.selection = &cli.WindowSelection{New: true}
		return
	}
	m.selection = &cli.WindowSelection{SessionID: current.id, Relaunch: current.state == "interrupted"}
	if current.state == "running" || m.picker.selectedSessionAttached() {
		m.selection.FullPicker = true
	}
}

func (m windowModel) applyForget(result sessionActionResultMsg) (tea.Model, tea.Cmd) {
	action := m.picker.sessionAction
	if action.phase != sessionActionRunning || result.generation != action.generation || result.target != action.target {
		return m, nil
	}
	m.picker.cancelAction = nil
	m.picker.sessionAction = sessionActionState{}
	if result.err != nil {
		m.picker.notice = "Forget failed: " + result.err.Error()
		return m, nil
	}
	rows := m.picker.currentHost().sessions
	next := make([]session, 0, len(rows))
	for _, current := range rows {
		if current.id != result.target.sessionID {
			next = append(next, current)
		}
	}
	m.picker.hosts[0].sessions = next
	m.picker.notice = ""
	command := m.picker.list.SetItems(sessionItems(next))
	m.picker.list.Select(0)
	m.selected = len(next) > 0 && next[0].state != "running"
	inspection := m.picker.restartInspectionLoop()
	m.resize()
	return m, tea.Batch(command, inspection)
}

func (m *windowModel) resize() {
	rows := min(9, max(1, len(m.picker.currentHost().sessions)), max(1, m.picker.height-9))
	m.picker.list.SetSize(m.picker.width, rows)
	m.picker.list.SetDelegate(windowDelegate{sessionDelegate: sessionDelegate{
		styles: m.picker.styles, now: m.picker.now, hostAlias: m.picker.currentHost().alias,
		inspection: m.picker.inspection, summaries: m.picker.summaries, hostNames: m.hostNames,
	}, selected: m.selected})
}

func (m windowModel) View() tea.View {
	picker := m.picker
	header := picker.styles.title.Render("mesh") + picker.styles.muted.Render("  Resume on "+safeText(picker.currentHost().alias))
	lines := []string{header, ""}
	lines = append(lines, picker.listViewRows(picker.list.Height())...)
	lines = append(lines, "")
	if m.selected {
		_, current, ok := picker.currentSession()
		if ok {
			details := picker.detailsFor(current)
			if endedSession(current) {
				lines = append(lines, picker.styles.muted.Render(recoveryActionLabel(current)+" in "+details.directory+". Press l for Restart command."))
				lines = append(lines, picker.styles.muted.Render(details.screenStatus))
			}
			preview := details.preview
			preview = preview[max(0, len(preview)-3):]
			for _, line := range preview {
				lines = append(lines, "  "+line)
			}
		}
	}
	if picker.notice != "" {
		lines = append(lines, picker.styles.warning.Render(safeText(picker.notice)))
	}
	action := "resume"
	if !m.selected {
		action = "new"
	} else if _, current, ok := picker.currentSession(); ok {
		if current.state == "interrupted" {
			action = recoveryActionLabel(current)
		} else if current.state == "running" || picker.selectedSessionAttached() {
			action = "open picker"
		}
	}
	footer := picker.styles.hints(hint{"enter", action}, hint{"1-9", "pick"}, hint{"s", "shell"}, hint{"n", "new"}, hint{"l", "list"}, hint{"x", "forget"}, hint{"esc", "cancel"})
	lines = append(lines[:min(len(lines), max(0, picker.height-1))], footer)
	for index := range lines {
		lines[index] = truncate(lines[index], picker.width)
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.WindowTitle = "Mesh"
	return view
}

type windowDelegate struct {
	sessionDelegate
	selected bool
}

func (delegate windowDelegate) Render(output io.Writer, browser list.Model, index int, value list.Item) {
	item, ok := value.(sessionItem)
	if !ok {
		return
	}
	label := "  "
	if index < 9 {
		label = fmt.Sprintf("%d ", index+1)
	}
	_, _ = io.WriteString(output, delegate.styles.muted.Render(label))
	browser.SetWidth(max(1, browser.Width()-2))
	delegate.renderRow(output, browser, item.session, delegate.selected && index == browser.Index())
}
