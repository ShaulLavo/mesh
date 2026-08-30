// Package tui implements Mesh's interactive host and session picker.
package tui

import (
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type screen uint8

const (
	hostScreen screen = iota
	sessionScreen
)

type host struct {
	alias    string
	stale    bool
	sessions []session
}

type session struct {
	id        string
	state     string
	command   []string
	cwd       string
	createdAt time.Time
}

type selection interface{ pickerSelection() }

type cancelSelection struct{}

func (cancelSelection) pickerSelection() {}

type attachSelection struct {
	hostAlias string
	sessionID string
}

func (attachSelection) pickerSelection() {}

type newSelection struct{ hostAlias string }

func (newSelection) pickerSelection() {}

type resumeSelection struct{ hostAlias string }

func (resumeSelection) pickerSelection() {}

type wakeSelection struct{ hostAlias string }

func (wakeSelection) pickerSelection() {}

type model struct {
	hosts        []host
	screen       screen
	selectedHost int
	list         list.Model
	selection    selection
	now          time.Time
	width        int
	height       int
	notice       string
	styles       pickerStyles
}

const (
	defaultWidth  = 80
	defaultHeight = 24
	frameRows     = 5
)

func newModel(hosts []host, now time.Time) model {
	styles := newPickerStyles()
	items := hostItems(hosts)
	browser := list.New(items, hostDelegate{styles: styles}, defaultWidth, defaultHeight-frameRows)
	configureList(&browser)
	browser.SetStatusBarItemName("host", "hosts")
	return model{
		hosts:  cloneHosts(hosts),
		list:   browser,
		now:    now,
		width:  defaultWidth,
		height: defaultHeight,
		styles: styles,
	}
}

func configureList(browser *list.Model) {
	browser.SetFilteringEnabled(false)
	browser.SetShowTitle(false)
	browser.SetShowFilter(false)
	browser.SetShowStatusBar(false)
	browser.SetShowPagination(false)
	browser.SetShowHelp(false)
	browser.DisableQuitKeybindings()
}

func cloneHosts(hosts []host) []host {
	cloned := make([]host, len(hosts))
	for index, current := range hosts {
		current.sessions = append([]session(nil), current.sessions...)
		for sessionIndex := range current.sessions {
			current.sessions[sessionIndex].command = append([]string(nil), current.sessions[sessionIndex].command...)
		}
		cloned[index] = current
	}
	return cloned
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = max(1, message.Width)
		m.height = max(1, message.Height)
		m.list.SetSize(m.width, max(1, m.height-frameRows))
		return m, nil
	case tea.KeyPressMsg:
		if command := m.handleKey(message); command != nil {
			return m, command
		}
		if m.selection != nil {
			return m, tea.Quit
		}
	}

	updated, command := m.list.Update(message)
	m.list = updated
	return m, command
}

func (m *model) handleKey(key tea.KeyPressMsg) tea.Cmd {
	switch key.String() {
	case "ctrl+c", "q":
		m.selection = cancelSelection{}
		return tea.Quit
	case "esc":
		if m.screen == sessionScreen {
			m.showHosts()
			return nil
		}
		m.selection = cancelSelection{}
		return tea.Quit
	case "enter":
		if m.screen == hostScreen {
			m.showSessions()
			return nil
		}
		m.attachSelected()
		if m.selection != nil {
			return tea.Quit
		}
		return nil
	case "n":
		if m.screen == sessionScreen {
			current := m.currentHost()
			if current.stale {
				m.notice = current.alias + " is offline; wake it first"
				return nil
			}
			m.selection = newSelection{hostAlias: current.alias}
			return tea.Quit
		}
	case "r":
		if m.screen == sessionScreen {
			current := m.currentHost()
			switch {
			case current.stale:
				m.notice = current.alias + " is offline; wake it first"
			case activeSessions(current.sessions) == 0:
				m.notice = current.alias + " has no active sessions"
			default:
				m.selection = resumeSelection{hostAlias: current.alias}
				return tea.Quit
			}
			return nil
		}
	case "w":
		if m.screen == sessionScreen {
			current := m.currentHost()
			if current.stale {
				m.selection = wakeSelection{hostAlias: current.alias}
				return tea.Quit
			}
			m.notice = current.alias + " is already online"
			return nil
		}
	}
	return nil
}

func (m *model) showSessions() {
	selected, ok := m.list.SelectedItem().(hostItem)
	if !ok {
		return
	}
	m.selectedHost = selected.index
	m.screen = sessionScreen
	m.notice = ""
	m.list.SetDelegate(sessionDelegate{styles: m.styles, now: m.now, stale: m.currentHost().stale})
	m.list.SetStatusBarItemName("session", "sessions")
	_ = m.list.SetItems(sessionItems(m.currentHost().sessions))
	m.list.ResetSelected()
}

func (m *model) showHosts() {
	m.screen = hostScreen
	m.notice = ""
	m.list.SetDelegate(hostDelegate{styles: m.styles})
	m.list.SetStatusBarItemName("host", "hosts")
	_ = m.list.SetItems(hostItems(m.hosts))
	m.list.Select(m.selectedHost)
}

func (m *model) attachSelected() {
	selected, ok := m.list.SelectedItem().(sessionItem)
	if !ok {
		return
	}
	if selected.session.state != "running" && selected.session.state != "detached" {
		m.notice = fmt.Sprintf("%s is %s; use mesh logs %s", selected.session.id, selected.session.state, selected.session.id)
		return
	}
	m.selection = attachSelection{hostAlias: m.currentHost().alias, sessionID: selected.session.id}
}

func (m model) currentHost() host {
	return m.hosts[m.selectedHost]
}

func (m model) View() tea.View {
	header, subtitle, footer := m.chrome()
	content := header + "\n" + subtitle + "\n\n" + m.list.View() + "\n" + footer
	view := tea.NewView(content)
	view.AltScreen = true
	view.WindowTitle = "Mesh"
	return view
}

func (m model) chrome() (string, string, string) {
	if m.screen == hostScreen {
		header := m.styles.title.Render("mesh")
		subtitle := "Choose a host."
		if len(m.hosts) == 0 {
			subtitle = "No hosts yet. Add one with mesh add [user@]host."
		}
		return truncate(header, m.width), truncate(m.styles.muted.Render(subtitle), m.width), truncate("↑/↓ move  enter sessions  esc cancel", m.width)
	}

	current := m.currentHost()
	status := fmt.Sprintf("online  %d active", activeSessions(current.sessions))
	if current.stale {
		status = "offline  cached-stale"
	}
	header := m.styles.title.Render("mesh / "+current.alias) + "  " + m.styles.status(current.stale).Render(status)
	subtitle := "Choose a session, or start another one."
	if current.stale {
		subtitle = "Cached sessions may be stale. Wake the host before starting work."
	}
	if m.notice != "" {
		subtitle = m.notice
	}
	footer := "↑/↓ move  enter attach  n new  r resume  esc hosts"
	if current.stale {
		footer = "↑/↓ move  enter try attach  w wake  esc hosts"
	}
	return truncate(header, m.width), truncate(m.styles.muted.Render(subtitle), m.width), truncate(footer, m.width)
}

func truncate(value string, width int) string {
	return ansi.Truncate(value, max(1, width), "…")
}

func activeSessions(sessions []session) int {
	count := 0
	for _, current := range sessions {
		if current.state == "running" || current.state == "detached" {
			count++
		}
	}
	return count
}

type hostItem struct {
	index int
	host  host
}

func (item hostItem) FilterValue() string { return item.host.alias }

func hostItems(hosts []host) []list.Item {
	items := make([]list.Item, len(hosts))
	for index, current := range hosts {
		items[index] = hostItem{index: index, host: current}
	}
	return items
}

type sessionItem struct{ session session }

func (item sessionItem) FilterValue() string {
	return strings.Join([]string{item.session.id, item.session.state, strings.Join(item.session.command, " "), item.session.cwd}, " ")
}

func sessionItems(sessions []session) []list.Item {
	items := make([]list.Item, len(sessions))
	for index, current := range sessions {
		items[index] = sessionItem{session: current}
	}
	return items
}

type hostDelegate struct{ styles pickerStyles }

func (delegate hostDelegate) Height() int  { return 2 }
func (delegate hostDelegate) Spacing() int { return 1 }
func (delegate hostDelegate) Update(tea.Msg, *list.Model) tea.Cmd {
	return nil
}

func (delegate hostDelegate) Render(output io.Writer, browser list.Model, index int, value list.Item) {
	item, ok := value.(hostItem)
	if !ok {
		return
	}
	selected := index == browser.Index()
	prefix := "  "
	if selected {
		prefix = delegate.styles.cursor.Render("› ")
	}
	status := "online"
	if item.host.stale {
		status = "offline  cached-stale"
	}
	first := fmt.Sprintf("%s%s  %s", prefix, delegate.styles.item(selected).Render(item.host.alias), delegate.styles.status(item.host.stale).Render(status))
	second := fmt.Sprintf("  %d active  %d sessions", activeSessions(item.host.sessions), len(item.host.sessions))
	_, _ = fmt.Fprintf(output, "%s\n%s", truncate(first, browser.Width()), truncate(delegate.styles.muted.Render(second), browser.Width()))
}

type sessionDelegate struct {
	styles pickerStyles
	now    time.Time
	stale  bool
}

func (delegate sessionDelegate) Height() int  { return 3 }
func (delegate sessionDelegate) Spacing() int { return 1 }
func (delegate sessionDelegate) Update(tea.Msg, *list.Model) tea.Cmd {
	return nil
}

func (delegate sessionDelegate) Render(output io.Writer, browser list.Model, index int, value list.Item) {
	item, ok := value.(sessionItem)
	if !ok {
		return
	}
	selected := index == browser.Index()
	prefix := "  "
	if selected {
		prefix = delegate.styles.cursor.Render("› ")
	}
	source := "live"
	if delegate.stale {
		source = "cached-stale"
	}
	first := fmt.Sprintf("%s%s  %-11s  %4s  %s", prefix, delegate.styles.item(selected).Render(item.session.id), item.session.state, age(delegate.now, item.session.createdAt), delegate.styles.status(delegate.stale).Render(source))
	command := strings.Join(item.session.command, " ")
	if command == "" {
		command = "command unknown"
	}
	cwd := item.session.cwd
	if cwd == "" {
		cwd = "cwd unknown"
	}
	_, _ = fmt.Fprintf(output, "%s\n%s\n%s", truncate(first, browser.Width()), truncate(delegate.styles.command.Render("  "+command), browser.Width()), truncate(delegate.styles.muted.Render("  "+cwd), browser.Width()))
}

func age(now, created time.Time) string {
	duration := now.Sub(created).Round(time.Second)
	if duration < 0 {
		duration = 0
	}
	switch {
	case duration < time.Minute:
		return fmt.Sprintf("%ds", int(duration.Seconds()))
	case duration < time.Hour:
		return fmt.Sprintf("%dm", int(duration.Minutes()))
	case duration < 24*time.Hour:
		return fmt.Sprintf("%dh", int(duration.Hours()))
	default:
		return fmt.Sprintf("%dd", int(duration.Hours()/24))
	}
}

type pickerStyles struct {
	title   lipgloss.Style
	cursor  lipgloss.Style
	accent  lipgloss.Style
	warning lipgloss.Style
	muted   lipgloss.Style
	command lipgloss.Style
}

func newPickerStyles() pickerStyles {
	return pickerStyles{
		title:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#C4A7FF")),
		cursor:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#9D7CFF")),
		accent:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E8DEFF")),
		warning: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFB86C")),
		muted:   lipgloss.NewStyle().Faint(true),
		command: lipgloss.NewStyle().Foreground(lipgloss.Color("#D8DEE9")),
	}
}

func (styles pickerStyles) item(selected bool) lipgloss.Style {
	if selected {
		return styles.accent
	}
	return lipgloss.NewStyle()
}

func (styles pickerStyles) status(stale bool) lipgloss.Style {
	if stale {
		return styles.warning
	}
	return styles.muted
}
