package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/shaul/mesh/internal/cli"
)

type updateSelection struct{}

func (updateSelection) pickerSelection() {}

type updateNoticeState struct {
	value      cli.UpdateNotice
	callbacks  cli.UpdateNoticeCallbacks
	generation uint64
	dismissing bool
	problem    string
}

type updateNoticeResultMsg struct {
	generation uint64
	value      cli.UpdateNotice
}

type updateNoticeDismissedMsg struct {
	generation uint64
	previous   cli.UpdateNotice
	err        error
}

func (m *model) configureUpdateNotice(callbacks cli.UpdateNoticeCallbacks) {
	m.updateNotice.callbacks = callbacks
	if callbacks.Cached != nil {
		m.updateNotice.value = callbacks.Cached()
	}
	m.resizeList()
}

func (m model) refreshUpdateNotice() tea.Cmd {
	if m.updateNotice.callbacks.Refresh == nil {
		return nil
	}
	return func() tea.Msg {
		return updateNoticeResultMsg{generation: m.updateNotice.generation, value: m.updateNotice.callbacks.Refresh(m.ctx)}
	}
}

func (m *model) handleUpdateNoticeKey(key tea.KeyPressMsg) (bool, tea.Cmd) {
	value := m.updateNotice.value
	if value.Version == "" && value.Pending == "" {
		return false, nil
	}
	if key.String() != "u" && key.String() != "d" && key.String() != "v" {
		return false, nil
	}
	if sessionActionBusy(m.sessionAction) || m.updateNotice.dismissing {
		return true, nil
	}
	if key.String() == "u" {
		m.selection = updateSelection{}
		m.invalidateSessionScreenLoops()
		return true, tea.Quit
	}
	if value.Version == "" || m.updateNotice.callbacks.Dismiss == nil {
		return true, nil
	}
	return true, m.dismissUpdateNotice(key.String() == "v")
}

func (m *model) dismissUpdateNotice(skip bool) tea.Cmd {
	previous := m.updateNotice.value
	m.updateNotice.generation++
	m.updateNotice.dismissing = true
	m.updateNotice.problem = ""
	m.updateNotice.value.Version = ""
	m.resizeList()
	generation := m.updateNotice.generation
	dismiss := m.updateNotice.callbacks.Dismiss
	ctx := m.ctx
	return func() tea.Msg {
		err := dismiss(ctx, cli.UpdateNoticeDismissal{Version: previous.Version, Skip: skip})
		return updateNoticeDismissedMsg{generation: generation, previous: previous, err: err}
	}
}

func (m model) applyUpdateNotice(result updateNoticeResultMsg) model {
	if result.generation != m.updateNotice.generation {
		return m
	}
	m.updateNotice.value = result.value
	m.resizeList()
	return m
}

func (m model) applyUpdateNoticeDismissal(result updateNoticeDismissedMsg) model {
	if result.generation != m.updateNotice.generation {
		return m
	}
	m.updateNotice.dismissing = false
	if result.err != nil {
		m.updateNotice.value = result.previous
		m.updateNotice.problem = "Could not save update reminder: " + result.err.Error()
	}
	m.resizeList()
	return m
}

func (m model) updateNoticeLines() []string {
	if m.height < 7 || m.fullPreview {
		return nil
	}
	var lines []string
	value := m.updateNotice.value
	if value.Version != "" {
		lines = append(lines, m.styles.accent.Render(fmt.Sprintf("Mesh %s is available.", safeText(value.Version))))
	}
	if value.Pending != "" {
		lines = append(lines, m.styles.warning.Render(safeText(value.Pending)))
	}
	if len(lines) > 0 {
		hints := []hint{{"u", "Review update"}}
		if value.Version != "" && m.updateNotice.callbacks.Dismiss != nil {
			hints = append(hints, hint{"d", "Remind me tomorrow"}, hint{"v", "Skip this version"})
		}
		lines = append(lines, m.styles.hints(hints...))
	}
	if m.updateNotice.problem != "" {
		lines = append(lines, m.styles.warning.Render(safeText(m.updateNotice.problem)))
	}
	for index := range lines {
		lines[index] = truncate(lines[index], m.width)
	}
	return lines[:min(len(lines), max(0, m.height-4))]
}
