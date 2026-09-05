// Package tui implements Mesh's interactive host and session picker.
package tui

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
)

type screen uint8

const (
	hostScreen screen = iota
	sessionScreen
)

type host struct {
	id          string
	alias       string
	route       string
	stale       bool
	local       bool
	sessions    []session
	served      []servedWebsite
	servedKnown bool
	servedStale bool
}

type servedWebsite struct {
	url    string
	health string
	stale  bool
}

type session struct {
	id              string
	state           string
	command         []string
	cwd             string
	createdAt       time.Time
	recovery        *recovery.Record
	recoveryError   string
	replacementID   string
	recoveredFrom   string
	previousAttempt bool
}

type selection interface{ pickerSelection() }

type cancelSelection struct{}

func (cancelSelection) pickerSelection() {}

type attachSelection struct {
	hostAlias      string
	sessionID      string
	relaunch       bool
	takeOver       bool
	recoveryAction recovery.Action
}

func (attachSelection) pickerSelection() {}

type newSelection struct{ hostAlias string }

func (newSelection) pickerSelection() {}

type resumeSelection struct{ hostAlias string }

func (resumeSelection) pickerSelection() {}

type wakeSelection struct{ hostAlias string }

type initialSessionRefreshMsg struct{}

func (wakeSelection) pickerSelection() {}

type model struct {
	hosts              []host
	screen             screen
	selectedHost       int
	list               list.Model
	selection          selection
	now                time.Time
	width              int
	height             int
	notice             string
	styles             pickerStyles
	ctx                context.Context
	inspect            cli.PickerInspectFunc
	refresh            cli.PickerRefreshFunc
	act                cli.PickerSessionActionFunc
	inspection         inspectionState
	summaries          map[inspectionTarget]sessionLiveSummary
	sessionAction      sessionActionState
	inspectSeq         uint64
	summarySeq         uint64
	actionSeq          uint64
	refreshEpoch       uint64
	catalogEpoch       uint64
	fullPreview        bool
	containingSessions map[containingSessionKey]containingSessionState
	cancelInspect      context.CancelFunc
	cancelSummary      context.CancelFunc
	cancelRefresh      context.CancelFunc
	cancelAction       context.CancelFunc
	refreshOnInit      bool
	loadHosts          func(context.Context) ([]cli.HostSessions, error)
	containingPath     []protocol.SessionIdentity
}

const (
	defaultWidth  = 80
	defaultHeight = 24
	frameRows     = 5
)

func newModel(hosts []host, now time.Time) model {
	return newInspectingModel(context.Background(), hosts, nil, now)
}

func newInspectingModel(ctx context.Context, hosts []host, inspect cli.PickerInspectFunc, now time.Time) model {
	if ctx == nil {
		ctx = context.Background()
	}
	styles := newPickerStyles()
	items := hostItems(hosts)
	browser := list.New(items, hostDelegate{styles: styles}, defaultWidth, defaultHeight-frameRows)
	configureList(&browser)
	browser.SetStatusBarItemName("host", "hosts")
	return model{
		hosts:              cloneHosts(hosts),
		list:               browser,
		now:                now,
		width:              defaultWidth,
		height:             defaultHeight,
		styles:             styles,
		ctx:                ctx,
		inspect:            inspect,
		summaries:          make(map[inspectionTarget]sessionLiveSummary),
		containingSessions: make(map[containingSessionKey]containingSessionState),
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
	browser.InfiniteScrolling = true
}

func cloneHosts(hosts []host) []host {
	cloned := make([]host, len(hosts))
	for index, current := range hosts {
		current.sessions = append([]session(nil), current.sessions...)
		current.served = append([]servedWebsite(nil), current.served...)
		for sessionIndex := range current.sessions {
			current.sessions[sessionIndex].command = append([]string(nil), current.sessions[sessionIndex].command...)
			current.sessions[sessionIndex].recovery = cloneRecovery(current.sessions[sessionIndex].recovery)
		}
		cloned[index] = current
	}
	return cloned
}

func (m model) Init() tea.Cmd {
	var refresh tea.Cmd
	if m.refreshOnInit {
		refresh = func() tea.Msg { return initialSessionRefreshMsg{} }
	}
	return tea.Batch(refresh, m.loadHostCatalog())
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width = max(1, message.Width)
		m.height = max(1, message.Height)
		m.resizeList()
		if m.screen == sessionScreen {
			startCatalog := m.refreshOnInit
			m.refreshOnInit = false
			if startCatalog {
				return m, m.restartSessionScreenLoops()
			}
			return m, m.restartInspectionLoop()
		}
		return m, nil
	case initialSessionRefreshMsg:
		if !m.refreshOnInit || m.screen != sessionScreen {
			return m, nil
		}
		m.refreshOnInit = false
		return m, m.restartSessionScreenLoops()
	case tea.KeyPressMsg:
		if handled, command := m.handleKey(message); handled {
			if m.selection != nil && command == nil {
				command = tea.Quit
			}
			if m.selection != nil {
				m.invalidateSessionScreenLoops()
			}
			return m, command
		}
	case inspectionResultMsg:
		return m.applyInspection(message), nil
	case sessionSummariesResultMsg:
		return m.applySessionSummaries(message), nil
	case catalogRefreshResultMsg:
		return m.applyCatalogRefresh(message)
	case hostCatalogLoadedMsg:
		return m.applyLoadedHosts(message)
	case sessionActionResultMsg:
		return m.applySessionAction(message)
	case catalogRefreshTickMsg:
		if m.screen != sessionScreen || message.epoch != m.catalogEpoch {
			return m, nil
		}
		m.now = message.at
		m.refreshSessionDelegate()
		return m, m.refreshSelectedHost(message.epoch)
	case inspectionTickMsg:
		if m.screen != sessionScreen || message.epoch != m.refreshEpoch {
			return m, nil
		}
		m.now = message.at
		m.refreshSessionDelegate()
		inspect := m.inspectSelected()
		summaries := m.inspectVisibleSessionSummaries()
		return m, tea.Batch(inspect, summaries, m.scheduleInspectionTick(message.epoch))
	}

	before := m.selectedSessionID()
	updated, command := m.list.Update(message)
	m.list = updated
	if m.screen == sessionScreen && m.selectedSessionID() != before {
		if !sessionActionBusy(m.sessionAction) {
			m.notice = ""
			if m.sessionAction.phase == sessionActionFailed {
				m.sessionAction = sessionActionState{}
			}
		}
		m.fullPreview = false
		command = tea.Batch(command, m.restartInspectionLoop())
	}
	return m, command
}

func (m *model) handleKey(key tea.KeyPressMsg) (bool, tea.Cmd) {
	if sessionActionBusy(m.sessionAction) {
		switch key.String() {
		case "enter", "n", "r", "k", "x", "w", "s", "c":
			if m.notice == "" {
				m.notice = sessionActionNotice(m.sessionAction)
			}
			return true, nil
		}
	}
	switch key.String() {
	case "s", "c":
		return m.selectRecoveryAction(key.String())
	case "ctrl+c", "q":
		m.selection = cancelSelection{}
		return true, tea.Quit
	case "esc":
		if m.screen == sessionScreen {
			if m.fullPreview {
				m.fullPreview = false
				return true, nil
			}
			m.showHosts()
			return true, nil
		}
		m.selection = cancelSelection{}
		return true, tea.Quit
	case "enter":
		if m.screen == hostScreen {
			return true, m.showSessions()
		}
		m.attachSelected()
		if m.selection != nil {
			return true, tea.Quit
		}
		return true, nil
	case "space":
		if m.screen == sessionScreen {
			if _, _, ok := m.currentSession(); ok {
				m.fullPreview = !m.fullPreview
			}
			return true, nil
		}
	case "n":
		if m.screen == sessionScreen {
			if m.fullPreview {
				return true, nil
			}
			current := m.currentHost()
			if current.stale {
				m.notice = current.alias + " is offline; wake it first"
				return true, nil
			}
			m.selection = newSelection{hostAlias: current.alias}
			return true, tea.Quit
		}
	case "r":
		if m.screen == sessionScreen {
			if m.fullPreview {
				return true, nil
			}
			current := m.currentHost()
			switch {
			case current.stale:
				m.notice = current.alias + " is offline; wake it first"
			case activeSessions(current.sessions) == 0:
				m.notice = current.alias + " has no active sessions"
			default:
				m.selection = resumeSelection{hostAlias: current.alias}
				return true, tea.Quit
			}
			return true, nil
		}
	case "k":
		if m.screen == sessionScreen {
			if m.fullPreview {
				return true, nil
			}
			current, session, ok := m.currentSession()
			if !ok {
				return true, nil
			}
			if current.stale {
				m.notice = current.alias + " is offline; wake it first"
				return true, nil
			}
			if session.state != "running" && session.state != "detached" {
				m.notice = session.id + " is already " + session.state
				return true, nil
			}
			return true, m.startSessionAction(cli.PickerKillSession)
		}
	case "x":
		if m.screen == sessionScreen {
			if m.fullPreview {
				return true, nil
			}
			current, session, ok := m.currentSession()
			if !ok {
				return true, nil
			}
			if current.stale {
				m.notice = current.alias + " is offline; wake it first"
				return true, nil
			}
			if session.state == "running" || session.state == "detached" {
				m.notice = session.id + " is still " + session.state + "; kill it first"
				return true, nil
			}
			return true, m.startSessionAction(cli.PickerRemoveSession)
		}
	case "w":
		if m.screen == sessionScreen {
			if m.fullPreview {
				return true, nil
			}
			current := m.currentHost()
			if current.stale {
				m.selection = wakeSelection{hostAlias: current.alias}
				return true, tea.Quit
			}
			m.notice = current.alias + " is already online"
			return true, nil
		}
	}
	if m.screen == sessionScreen && m.fullPreview {
		return true, nil
	}
	return false, nil
}

func (m *model) showSessions() tea.Cmd {
	selected, ok := m.list.SelectedItem().(hostItem)
	if !ok {
		return nil
	}
	m.refreshOnInit = false
	m.enterSessions(selected.index)
	return m.restartSessionScreenLoops()
}

func (m *model) enterSessions(hostIndex int) {
	m.selectedHost = hostIndex
	m.screen = sessionScreen
	m.notice = ""
	m.list.SetDelegate(sessionDelegate{styles: m.styles, now: m.now})
	m.list.SetStatusBarItemName("session", "sessions")
	_ = m.list.SetItems(sessionItems(m.currentHost().sessions))
	m.list.ResetSelected()
	m.fullPreview = false
	m.resizeList()
	if len(m.currentHost().sessions) > 0 {
		m.inspection = inspectionState{kind: inspectionLoading}
	} else {
		m.inspection = inspectionState{kind: inspectionUnavailable, problem: "No session is selected."}
	}
}

func (m *model) openHostSessions(alias string) bool {
	for index := range m.hosts {
		if m.hosts[index].alias != alias {
			continue
		}
		m.list.Select(index)
		m.enterSessions(index)
		m.refreshOnInit = true
		return true
	}
	return false
}

func (m *model) showHosts() {
	m.invalidateSessionScreenLoops()
	m.refreshOnInit = false
	m.screen = hostScreen
	m.notice = ""
	m.fullPreview = false
	m.inspection = inspectionState{}
	m.list.SetDelegate(hostDelegate{styles: m.styles})
	m.list.SetStatusBarItemName("host", "hosts")
	_ = m.list.SetItems(hostItems(m.hosts))
	m.list.Select(m.selectedHost)
	m.resizeList()
}

func (m *model) attachSelected() {
	selected, ok := m.list.SelectedItem().(sessionItem)
	if !ok {
		return
	}
	if selected.session.state != "running" && selected.session.state != "detached" && !endedSession(selected.session) {
		m.notice = fmt.Sprintf("%s is %s; use mesh logs %s", selected.session.id, selected.session.state, selected.session.id)
		return
	}
	m.selection = attachSelection{
		hostAlias: m.currentHost().alias, sessionID: selected.session.id,
		relaunch: endedSession(selected.session), takeOver: m.selectedSessionAttached(),
	}
}

func (m model) currentHost() host {
	return m.hosts[m.selectedHost]
}

func (m model) View() tea.View {
	if m.screen == sessionScreen {
		return m.sessionView()
	}
	header, subtitle, footer := m.chrome()
	content := header + "\n" + subtitle + "\n\n" + m.list.View() + "\n" + footer
	view := tea.NewView(content)
	view.AltScreen = true
	view.WindowTitle = "Mesh"
	return view
}

func (m model) chrome() (string, string, string) {
	if m.screen == hostScreen {
		header := justify(m.styles.title.Render("mesh"), m.styles.muted.Render(count(len(m.hosts), "host")), m.width)
		subtitle := "Choose a host."
		if len(m.hosts) == 0 {
			subtitle = "No hosts yet. Add one with mesh add [user@]host."
		}
		footer := m.styles.hints(hint{"↑/↓", "move"}, hint{"enter", "sessions"}, hint{"esc", "cancel"})
		return truncate(header, m.width), truncate(m.styles.muted.Render(subtitle), m.width), truncate(footer, m.width)
	}

	current := m.currentHost()
	breadcrumb := m.styles.title.Render("mesh") + m.styles.muted.Render(" › ") + m.styles.accent.Render(safeText(current.alias))
	if route := safeText(current.route); route != "" && route != safeText(current.alias) {
		breadcrumb += m.styles.muted.Render("  " + route)
	}
	header := justify(breadcrumb, m.hostStatus(current), m.width)
	subtitle := m.styles.muted.Render("Choose a session, or start another one.")
	if current.stale {
		subtitle = m.styles.muted.Render("Cached sessions may be stale. Wake the host before starting work.")
	}
	if m.notice != "" {
		subtitle = m.styles.warning.Render(safeText(m.notice))
	}
	return truncate(header, m.width), truncate(subtitle, m.width), truncate(m.footer(current), m.width)
}

// hostStatus is the right-aligned half of the session header: liveness first,
// then how much is going on, so a glance says whether attaching will work.
func (m model) hostStatus(current host) string {
	parts := []string{m.styles.live.Render("●") + m.styles.muted.Render(" online"), m.styles.muted.Render(fmt.Sprintf("%d active", activeSessions(current.sessions)))}
	if current.stale {
		parts = []string{m.styles.warning.Render("▲ offline  ·  cached")}
	}
	if m.refresh != nil {
		parts = append(parts, m.styles.muted.Render(servedSummary(current)))
	}
	return strings.Join(parts, m.styles.muted.Render("  ·  "))
}

func (m model) footer(current host) string {
	action := "attach"
	if _, selected, ok := m.currentSession(); ok {
		if m.selectedSessionAttached() {
			action = "take over"
		}
		if endedSession(selected) {
			action = recoveryActionLabel(selected)
		}
	}
	if current.stale {
		action = "try attach"
	}
	busy := sessionActionBusy(m.sessionAction)
	if m.fullPreview {
		hints := []hint{{"space", "details"}, {"enter", action}, {"esc", "details"}}
		if busy {
			hints = []hint{{"space", "details"}, {"", "session update in progress"}, {"esc", "details"}}
		}
		return m.styles.hints(hints...)
	}
	if busy {
		return m.styles.hints(hint{"↑/↓", "browse"}, hint{"space", "screen"}, hint{"", "session update in progress"}, hint{"esc", "cancel"})
	}
	if current.stale {
		return m.styles.hints(hint{"↑/↓", ""}, hint{"enter", action}, hint{"space", "screen"}, hint{"w", "wake"}, hint{"esc", "hosts"})
	}
	if _, selected, ok := m.currentSession(); ok && endedSession(selected) {
		return m.styles.hints(hint{"enter", action}, hint{"s", "Open shell"}, hint{"c", "Restart command"}, hint{"space", "output"}, hint{"x", "forget"}, hint{"esc", "hosts"})
	}
	return m.styles.hints(
		hint{"↑/↓", ""}, hint{"enter", action}, hint{"space", "screen"},
		hint{"n", "new"}, hint{"r", "resume"}, hint{"k", "kill"}, hint{"x", "rm"}, hint{"esc", "hosts"},
	)
}

func truncate(value string, width int) string {
	return ansi.Truncate(value, max(1, width), "…")
}

// cell fits text into a fixed column so rows line up regardless of how long
// each value is. Styled text is fine: widths ignore escape sequences.
func cell(text string, width int) string {
	if width <= 0 {
		return ""
	}
	text = ansi.Truncate(text, width, "…")
	return text + strings.Repeat(" ", max(0, width-ansi.StringWidth(text)))
}

func justify(left, right string, width int) string {
	gap := width - ansi.StringWidth(left) - ansi.StringWidth(right)
	if gap < 2 {
		return truncate(left+"  "+right, width)
	}
	return left + strings.Repeat(" ", gap) + right
}

func count(quantity int, noun string) string {
	if quantity == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", quantity, noun)
}

func activeSessions(sessions []session) int {
	total := 0
	for _, current := range sessions {
		if current.state == "running" || current.state == "detached" {
			total++
		}
	}
	return total
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
	return strings.Join([]string{item.session.id, item.session.state, cli.SafeTerminalText(strings.Join(item.session.command, " ")), cli.SafeTerminalText(item.session.cwd)}, " ")
}

func sessionItems(sessions []session) []list.Item {
	items := make([]list.Item, len(sessions))
	for index, current := range sessions {
		items[index] = sessionItem{session: current}
	}
	return items
}

type hostDelegate struct{ styles pickerStyles }

func (delegate hostDelegate) Height() int  { return 1 }
func (delegate hostDelegate) Spacing() int { return 0 }
func (delegate hostDelegate) Update(tea.Msg, *list.Model) tea.Cmd {
	return nil
}

func (delegate hostDelegate) Render(output io.Writer, browser list.Model, index int, value list.Item) {
	item, ok := value.(hostItem)
	if !ok {
		return
	}
	aliasWidth, routeWidth := 0, 0
	for _, entry := range browser.Items() {
		other, ok := entry.(hostItem)
		if !ok {
			continue
		}
		aliasWidth = max(aliasWidth, ansi.StringWidth(safeText(other.host.alias)))
		routeWidth = max(routeWidth, ansi.StringWidth(safeText(other.host.route)))
	}
	aliasWidth = min(aliasWidth, 24)
	routeWidth = min(routeWidth, 32)

	selected := index == browser.Index()
	cursor := "  "
	if selected {
		cursor = delegate.styles.cursor.Render("› ")
	}
	glyph := delegate.styles.live.Render("●")
	status := delegate.styles.muted.Render(fmt.Sprintf("%d active  ·  %s", activeSessions(item.host.sessions), count(len(item.host.sessions), "session")))
	if item.host.stale {
		glyph = delegate.styles.warning.Render("▲")
		status = delegate.styles.warning.Render("offline  ·  cached") + delegate.styles.muted.Render("  ·  "+count(len(item.host.sessions), "session"))
	}
	row := cursor + glyph + " " + cell(delegate.styles.item(selected).Render(safeText(item.host.alias)), aliasWidth)
	if routeWidth > 0 {
		row += "  " + cell(delegate.styles.muted.Render(safeText(item.host.route)), routeWidth)
	}
	row += "  " + status
	_, _ = fmt.Fprint(output, truncate(row, browser.Width()))
}

type sessionDelegate struct {
	styles     pickerStyles
	now        time.Time
	hostAlias  string
	inspection inspectionState
	summaries  map[inspectionTarget]sessionLiveSummary
	hostNames  map[string]string
	via        map[string]string
}

func (delegate sessionDelegate) Height() int  { return 1 }
func (delegate sessionDelegate) Spacing() int { return 0 }
func (delegate sessionDelegate) Update(tea.Msg, *list.Model) tea.Cmd {
	return nil
}

// sessionRow is one list line before layout. The terminal title leads when the
// host reports one, because that is what the program itself calls the work;
// the project label then becomes a quieter hint about where it runs.
type sessionRow struct {
	primary    string
	secondary  string
	context    string
	glyphState string
	state      string
	activity   string
	id         string
}

type sessionColumns struct{ state, activity, id int }

const minimumHeadlineColumns = 16

func (delegate sessionDelegate) Render(output io.Writer, browser list.Model, index int, value list.Item) {
	item, ok := value.(sessionItem)
	if !ok {
		return
	}
	delegate.renderRow(output, browser, item.session, index == browser.Index())
}

func (delegate sessionDelegate) renderRow(output io.Writer, browser list.Model, current session, selected bool) {
	row := delegate.row(current, selected)
	columns := delegate.columns(browser)
	width := browser.Width()

	showState, showActivity := true, true
	headline := width - 4 - columns.id - 2 - columns.state - 2 - columns.activity - 2
	if headline < minimumHeadlineColumns {
		showActivity = false
		headline += columns.activity + 2
	}
	if headline < minimumHeadlineColumns {
		showState = false
		headline += columns.state + 2
	}

	cursor := "  "
	if selected {
		cursor = delegate.styles.cursor.Render("› ")
	}
	title := delegate.styles.item(selected).Render(row.primary)
	if row.secondary != "" {
		title += "  " + delegate.styles.muted.Render(row.secondary)
	}
	var line strings.Builder
	line.WriteString(cursor)
	line.WriteString(delegate.styles.stateGlyph(row.glyphState))
	line.WriteString(" ")
	if row.context == "" {
		line.WriteString(cell(title, max(1, headline)))
	} else {
		context := ansi.Truncate(row.context, max(1, headline-10), "…")
		line.WriteString(cell(title, max(1, headline-2-ansi.StringWidth(context))))
		line.WriteString("  " + delegate.styles.muted.Render(context))
	}
	if showState {
		line.WriteString("  " + cell(delegate.styles.stateText(row.state), columns.state))
	}
	if showActivity {
		line.WriteString("  " + cell(delegate.styles.muted.Render(row.activity), columns.activity))
	}
	line.WriteString("  " + delegate.styles.muted.Render(row.id))
	_, _ = fmt.Fprint(output, truncate(line.String(), width))
}

func (delegate sessionDelegate) columns(browser list.Model) sessionColumns {
	var columns sessionColumns
	for index, entry := range browser.Items() {
		item, ok := entry.(sessionItem)
		if !ok {
			continue
		}
		row := delegate.row(item.session, index == browser.Index())
		columns.state = max(columns.state, ansi.StringWidth(row.state))
		columns.activity = max(columns.activity, ansi.StringWidth(row.activity))
		columns.id = max(columns.id, ansi.StringWidth(row.id))
	}
	return columns
}

func (delegate sessionDelegate) row(current session, selected bool) sessionRow {
	row := sessionRow{
		primary:    sessionLabel(current),
		glyphState: current.state,
		state:      catalogSessionState(current.state),
		activity:   startedAge(delegate.now, current.createdAt),
		id:         safeText(current.id),
	}
	if current.recovery != nil {
		row.primary, row.secondary = sessionHeadline(sessionLabel(current), current.recovery.Title)
		if endedSession(current) && !current.recovery.CheckpointAt.IsZero() {
			row.activity = "saved " + age(delegate.now, current.recovery.CheckpointAt)
		}
	}
	if current.previousAttempt {
		row.primary = "↳ " + row.primary
		row.context = "previous attempt"
	}
	target := inspectionTarget{hostAlias: delegate.hostAlias, sessionID: current.id}
	if summary, ok := delegate.summaries[target]; ok && !endedSession(current) {
		row.primary, row.secondary = sessionHeadline(summarizedSessionLabel(current, summary), summary.terminalTitle)
		row.context = nestedSessionLabel(summary.nested, delegate.hostNames)
		row.activity = rowActivity(delegate.now, summary.observedAt, summary.receivedAt, summary.lastOutputAt, current.createdAt)
	}
	if selected && !endedSession(current) && delegate.inspection.kind == inspectionReady && delegate.inspection.target == target {
		value := delegate.inspection.value
		row.primary, row.secondary = sessionHeadline(inspectedSessionLabel(current, value), value.TerminalTitle)
		row.context = nestedSessionLabel(value.Nested, delegate.hostNames)
		if !delegate.inspection.beforePicker {
			row.glyphState, row.state = "detached", "detached"
			if value.Attached {
				row.glyphState, row.state = "running", "in use"
			}
		}
		row.activity = rowActivity(delegate.now, value.ObservedAt, delegate.inspection.receivedAt, value.LastOutputAt, current.createdAt)
	}
	row.context = appendRowLabel(row.context, delegate.via[current.id])
	return row
}

func sessionHeadline(label, title string) (primary, secondary string) {
	title = strings.TrimSpace(safeText(title))
	if title == "" || title == label {
		return label, ""
	}
	return title, label
}

func startedAge(now, createdAt time.Time) string {
	return "started " + age(now, createdAt) + " ago"
}

// rowActivity prefers the last output the host observed, shifted by the time
// since that observation arrived. A session that has not printed yet falls
// back to its age so the column never reads as an error.
func rowActivity(now, observedAt, receivedAt time.Time, lastOutputAt *time.Time, createdAt time.Time) string {
	if lastOutputAt == nil {
		return startedAge(now, createdAt)
	}
	if observedAt.IsZero() {
		observedAt = now
	}
	if !receivedAt.IsZero() && now.After(receivedAt) {
		observedAt = observedAt.Add(now.Sub(receivedAt))
	}
	return outputAge(observedAt, lastOutputAt)
}

func catalogSessionState(state string) string {
	if state == "running" {
		return "in use"
	}
	return safeText(state)
}

func sessionLabel(current session) string {
	process := ""
	if len(current.command) > 0 {
		process = current.command[0]
	}
	directory := current.cwd
	if current.recovery != nil && current.recovery.ShellDirectory != "" {
		directory = current.recovery.ShellDirectory
	}
	return sessionLabelParts(directory, process)
}

func inspectedSessionLabel(current session, inspection cli.SessionInspection) string {
	return summarizedSessionLabel(current, sessionLiveSummary{
		currentDirectory:  inspection.CurrentDirectory,
		foregroundCommand: inspection.ForegroundCommand,
	})
}

func summarizedSessionLabel(current session, summary sessionLiveSummary) string {
	directory := summary.currentDirectory
	if directory == "" {
		directory = current.cwd
	}
	process := firstCommandWord(summary.foregroundCommand)
	if process == "" && len(current.command) > 0 {
		process = current.command[0]
	}
	return sessionLabelParts(directory, process)
}

func sessionLabelParts(directory, process string) string {
	project := ""
	if cwd := strings.TrimRight(safeText(directory), "/"); cwd != "" {
		project = path.Base(cwd)
		if project == "." || project == "/" {
			project = ""
		}
	}
	if process = safeText(process); process != "" {
		process = path.Base(process)
		if process == "." || process == "/" {
			process = ""
		}
	}
	switch {
	case project != "" && process != "":
		return project + " · " + process
	case project != "":
		return project
	case process != "":
		return process
	default:
		return "untitled session"
	}
}

func firstCommandWord(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func safeText(value string) string { return cli.SafeTerminalText(value) }

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
	key     lipgloss.Style
	live    lipgloss.Style
	border  lipgloss.Style
}

func newPickerStyles() pickerStyles {
	return pickerStyles{
		title:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#C4A7FF")),
		cursor:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#9D7CFF")),
		accent:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E8DEFF")),
		warning: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFB86C")),
		muted:   lipgloss.NewStyle().Faint(true),
		command: lipgloss.NewStyle().Foreground(lipgloss.Color("#D8DEE9")),
		key:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#C4A7FF")),
		live:    lipgloss.NewStyle().Foreground(lipgloss.Color("#A6E3A1")),
		border:  lipgloss.NewStyle().Faint(true),
	}
}

func (styles pickerStyles) item(selected bool) lipgloss.Style {
	if selected {
		return styles.accent
	}
	return lipgloss.NewStyle()
}

// stateGlyph varies shape as well as color so the column still reads on a
// monochrome terminal: filled for in use, hollow for waiting, a point for done.
func (styles pickerStyles) stateGlyph(state string) string {
	switch state {
	case "running":
		return styles.live.Render("●")
	case "detached":
		return styles.cursor.Render("○")
	case "interrupted":
		return styles.warning.Render("▲")
	default:
		return styles.muted.Render("·")
	}
}

func (styles pickerStyles) stateText(state string) string {
	if state == "interrupted" {
		return styles.warning.Render(state)
	}
	return styles.muted.Render(state)
}

// hint is one footer entry. An empty key renders a plain status phrase.
type hint struct{ key, label string }

func (styles pickerStyles) hints(hints ...hint) string {
	parts := make([]string, 0, len(hints))
	for _, current := range hints {
		switch {
		case current.key == "":
			parts = append(parts, styles.muted.Render(current.label))
		case current.label == "":
			parts = append(parts, styles.key.Render(current.key))
		default:
			parts = append(parts, styles.key.Render(current.key)+" "+styles.muted.Render(current.label))
		}
	}
	return strings.Join(parts, "  ")
}

// currentSession is the highlighted session and the host showing it.
func (m *model) currentSession() (host, session, bool) {
	current := m.currentHost()
	item, ok := m.list.SelectedItem().(sessionItem)
	if !ok {
		return current, session{}, false
	}
	return current, item.session, true
}
