package tui

import (
	"context"
	"fmt"
	"image/color"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
)

const (
	inspectionRefreshInterval = 2 * time.Second
	inspectionRequestTimeout  = 1500 * time.Millisecond
	inspectionPreviewColumns  = 160
	inspectionPreviewRows     = 24
	detailPanelBaseRows       = 7
)

type inspectionKind uint8

const (
	inspectionUnavailable inspectionKind = iota
	inspectionLoading
	inspectionReady
	inspectionFailed
)

type inspectionTarget struct {
	hostAlias string
	sessionID string
}

type containingSessionKey struct {
	hostID    string
	sessionID string
}

type containingSessionState struct {
	snapshot   *cli.SessionInspection
	receivedAt time.Time
}

type inspectionState struct {
	target       inspectionTarget
	generation   uint64
	kind         inspectionKind
	value        cli.SessionInspection
	receivedAt   time.Time
	hasValue     bool
	refreshing   bool
	beforePicker bool
	problem      string
}

type inspectionResultMsg struct {
	target     inspectionTarget
	generation uint64
	value      cli.SessionInspection
	err        error
}

type inspectionTickMsg struct {
	epoch uint64
	at    time.Time
}

// restartInspectionLoop invalidates any pending tick before it changes the
// selected inspection target. A late result also carries its own generation,
// so neither timers nor network responses can update a different row.
func (m *model) restartInspectionLoop() tea.Cmd {
	m.refreshEpoch++
	epoch := m.refreshEpoch
	inspect := m.inspectSelected()
	summaries := m.inspectVisibleSessionSummaries()
	m.refreshSessionDelegate()
	if inspect == nil && summaries == nil {
		if m.inspect == nil {
			return nil
		}
		return m.scheduleInspectionTick(epoch)
	}
	return tea.Batch(inspect, summaries, m.scheduleInspectionTick(epoch))
}

func (m *model) inspectSelected() tea.Cmd {
	m.stopInspection()
	host, current, ok := m.currentSession()
	if !ok {
		m.inspection = inspectionState{kind: inspectionUnavailable, problem: "No session is selected."}
		return nil
	}

	target := inspectionTarget{hostAlias: host.alias, sessionID: current.id}
	m.inspectSeq++
	generation := m.inspectSeq
	previous := m.inspection
	containing, beforePicker := m.containingSessionFor(target)
	next := inspectionState{target: target, generation: generation, beforePicker: beforePicker}
	if next.beforePicker {
		if containing.snapshot == nil {
			next.kind = inspectionUnavailable
			next.problem = "The screen before the picker opened could not be captured."
			m.inspection = next
			return nil
		}
		next.kind = inspectionReady
		next.value = cloneSessionInspection(*containing.snapshot)
		next.receivedAt = containing.receivedAt
		if next.receivedAt.IsZero() {
			next.receivedAt = m.now
			containing.receivedAt = next.receivedAt
			key := containingSessionKey{hostID: host.id, sessionID: current.id}
			m.containingSessions[key] = containing
		}
		next.hasValue = true
		m.inspection = next
		m.rememberSessionSummary(target, next.value)
		m.refreshSessionDelegate()
		return nil
	}

	switch {
	case host.stale:
		next.kind = inspectionUnavailable
		next.problem = "Live view unavailable while the host is offline."
		m.inspection = next
		return nil
	case current.state != "running" && current.state != "detached":
		next.kind = inspectionUnavailable
		next.problem = "Live view unavailable because this session has ended."
		m.inspection = next
		return nil
	case m.inspect == nil:
		next.kind = inspectionUnavailable
		next.problem = "Live view is unavailable in this client."
		m.inspection = next
		return nil
	}

	if previous.target == target && previous.hasValue {
		next.kind = inspectionReady
		next.value = previous.value
		next.receivedAt = previous.receivedAt
		next.hasValue = true
		next.refreshing = true
	} else {
		next.kind = inspectionLoading
	}
	m.inspection = next

	previewColumns := min(inspectionPreviewColumns, max(1, m.width-4))
	previewRows := inspectionPreviewRows
	request := cli.PickerInspectRequest{
		HostAlias:   host.alias,
		SessionID:   current.id,
		PreviewCols: previewColumns,
		PreviewRows: previewRows,
	}
	requestContext, cancel := context.WithTimeout(m.ctx, inspectionRequestTimeout)
	m.cancelInspect = cancel
	inspect := m.inspect
	return func() tea.Msg {
		defer cancel()
		value, err := inspect(requestContext, request)
		return inspectionResultMsg{target: target, generation: generation, value: value, err: err}
	}
}

func (m model) applyInspection(message inspectionResultMsg) model {
	if m.screen != sessionScreen || message.target != m.inspection.target || message.generation != m.inspection.generation {
		return m
	}
	m.cancelInspect = nil
	if message.err != nil {
		m.inspection.refreshing = false
		m.inspection.problem = message.err.Error()
		if m.inspection.hasValue {
			m.inspection.kind = inspectionReady
		} else {
			m.inspection.kind = inspectionFailed
		}
		m.refreshSessionDelegate()
		return m
	}
	value := message.value
	m.inspection.kind = inspectionReady
	m.inspection.value = value
	m.inspection.hasValue = true
	m.inspection.receivedAt = m.now
	m.inspection.refreshing = false
	m.inspection.problem = ""
	m.rememberSessionSummary(message.target, message.value)
	m.refreshSessionDelegate()
	return m
}

func (m *model) stopInspection() {
	if m.cancelInspect == nil {
		return
	}
	m.cancelInspect()
	m.cancelInspect = nil
}

func (m model) scheduleInspectionTick(epoch uint64) tea.Cmd {
	return tea.Tick(inspectionRefreshInterval, func(at time.Time) tea.Msg {
		return inspectionTickMsg{epoch: epoch, at: at}
	})
}

func (m *model) refreshSessionDelegate() {
	if m.screen != sessionScreen {
		return
	}
	m.list.SetDelegate(sessionDelegate{
		styles:     m.styles,
		now:        m.now,
		hostAlias:  m.currentHost().alias,
		inspection: m.inspection,
		summaries:  m.summaries,
		hostNames:  m.hostNames(),
		via:        m.sessionViaLabels(),
	})
}

func (m *model) resizeList() {
	if m.screen == hostScreen {
		m.list.SetSize(m.width, max(1, m.height-frameRows))
		return
	}
	listRows, _, _ := m.sessionLayout(max(0, m.height-4))
	m.list.SetSize(m.width, max(1, listRows))
}

func (m model) selectedSessionID() string {
	if m.screen != sessionScreen {
		return ""
	}
	item, ok := m.list.SelectedItem().(sessionItem)
	if !ok {
		return ""
	}
	return item.session.id
}

func (m model) sessionView() tea.View {
	header, subtitle, footer := m.chrome()
	if m.fullPreview {
		subtitle = truncate(m.styles.muted.Render(m.previewSubtitle()), m.width)
	}

	lines := make([]string, 0, m.height)
	switch m.height {
	case 1:
		lines = append(lines, header)
	case 2:
		lines = append(lines, header, footer)
	case 3:
		lines = append(lines, header, subtitle, footer)
	default:
		bodyRows := m.height - 4
		lines = append(lines, header, subtitle, "")
		if m.fullPreview {
			lines = append(lines, m.fullPreviewPanel(bodyRows)...)
		} else {
			lines = append(lines, m.sessionBody(bodyRows)...)
		}
		lines = append(lines, footer)
	}
	for index := range lines {
		lines[index] = truncate(lines[index], m.width)
	}

	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	view.WindowTitle = "Mesh"
	return view
}

func (m model) sessionBody(rows int) []string {
	listRows, previewRows, showPanel := m.sessionLayout(rows)
	servedRows := m.visibleServedWebsiteRows(rows)
	body := make([]string, 0, rows)
	body = append(body, m.listViewRows(listRows)...)
	body = append(body, servedRows...)
	if !showPanel {
		return fitRows(body, rows)
	}
	body = append(body, "")
	body = append(body, m.detailPanel(previewRows)...)
	return fitRows(body, rows)
}

func (m model) sessionLayout(bodyRows int) (listRows, previewRows int, showPanel bool) {
	availableRows := bodyRows - len(m.visibleServedWebsiteRows(bodyRows))
	desiredListRows := min(8, max(1, len(m.currentHost().sessions)))
	_, _, selected := m.currentSession()
	if !selected || availableRows < detailPanelBaseRows+2 {
		return min(max(1, availableRows), desiredListRows), 0, false
	}
	previewRows = min(inspectionPreviewRows, max(0, availableRows-1-detailPanelBaseRows-desiredListRows))
	listRows = availableRows - 1 - detailPanelBaseRows - previewRows
	if listRows < 1 {
		return min(max(1, availableRows), desiredListRows), 0, false
	}
	return listRows, previewRows, true
}

func (m model) listViewRows(rows int) []string {
	if rows <= 0 {
		return nil
	}
	view := strings.TrimSuffix(m.list.View(), "\n")
	lines := strings.Split(view, "\n")
	return fitRows(lines, rows)
}

func fitRows(lines []string, rows int) []string {
	if rows <= 0 {
		return nil
	}
	result := make([]string, rows)
	copy(result, lines[:min(len(lines), rows)])
	return result
}

type inspectionDetails struct {
	directoryLabel  string
	directory       string
	directorySource string
	foreground      string
	title           string
	attachment      string
	output          string
	screenStatus    string
	preview         []string
	previewStyled   bool
}

func (m model) detailsFor(current session) inspectionDetails {
	details := inspectionDetails{
		directoryLabel: "started in",
		directory:      safeText(current.cwd),
		foreground:     "loading",
		title:          "loading",
		attachment:     "catalog " + catalogSessionState(current.state),
		output:         "last output loading",
		screenStatus:   "loading live view",
		preview:        []string{"Loading current screen..."},
	}
	if details.directory == "" {
		details.directory = "unknown"
	}

	switch m.inspection.kind {
	case inspectionUnavailable:
		details.foreground = "unavailable"
		details.title = "unavailable"
		details.output = "last output unavailable"
		details.screenStatus = "live view unavailable"
		problem := m.inspection.problem
		if problem == "" {
			problem = "Live view is unavailable."
		}
		details.preview = []string{safeText(problem)}
	case inspectionFailed:
		details.foreground = "unavailable"
		details.title = "unavailable"
		details.output = "last output unavailable"
		details.screenStatus = "inspect failed, retrying"
		details.preview = []string{"Inspect failed: " + safeText(m.inspection.problem)}
	case inspectionReady:
		value := m.inspection.value
		if value.CurrentDirectory != "" {
			details.directoryLabel = "current path"
			details.directory = safeText(value.CurrentDirectory)
			switch value.DirectorySource {
			case cli.SessionDirectoryProcess:
				details.directorySource = "process"
			case cli.SessionDirectoryTerminal:
				details.directorySource = "terminal"
			}
		}
		details.foreground = safeText(value.ForegroundCommand)
		if details.foreground == "" {
			details.foreground = "unknown"
		}
		details.title = safeText(value.TerminalTitle)
		if details.title == "" {
			details.title = "unknown"
		}
		if m.inspection.beforePicker {
			if current.state == "running" {
				details.attachment = "attached"
			} else {
				details.attachment = safeText(current.state)
			}
		} else if value.Attached {
			details.attachment = "attached"
		} else {
			details.attachment = "detached"
		}
		details.output = inspectionOutputAge(m.now, m.inspection)
		details.screenStatus = "live"
		if m.inspection.beforePicker {
			details.screenStatus = "before picker"
		}
		if m.inspection.problem != "" {
			details.screenStatus = "last live view · refresh failed: " + safeText(m.inspection.problem)
		}
		details.preview, details.previewStyled = m.renderInspectionPreview(value)
		blank := true
		for _, line := range value.Preview {
			if strings.TrimSpace(line) != "" {
				blank = false
			}
		}
		if blank {
			details.preview = []string{"(screen is blank)"}
			details.previewStyled = false
		}
	}
	return details
}

const detailLabelColumns = 12

type detailFact struct{ label, value string }

func (m model) detailPanel(previewRows int) []string {
	_, current, ok := m.currentSession()
	if !ok {
		return nil
	}
	details := m.detailsFor(current)
	directory := details.directory
	if details.directorySource != "" {
		directory += "  ·  " + details.directorySource
	}
	launched := safeText(strings.Join(current.command, " "))
	if launched == "" {
		launched = "unknown"
	}
	facts := []detailFact{
		{details.directoryLabel, directory},
		{"foreground", details.foreground},
		{"launched", launched},
		{"activity", strings.Join([]string{details.attachment, details.output, startedAge(m.now, current.createdAt)}, "  ·  ")},
	}
	heading := m.sessionHeadline(current)
	if label := m.bestSessionLabel(current); label != heading {
		heading += "  ·  " + label
	}
	panel := []string{m.boxTop(heading+"  ·  "+safeText(current.id), m.width)}
	for _, fact := range facts {
		panel = append(panel, m.boxRow(m.styles.muted.Render(cell(fact.label, detailLabelColumns))+" "+m.factValue(fact.value), m.width))
	}
	screenStyle := m.styles.muted
	if m.inspection.kind == inspectionFailed || m.inspection.problem != "" {
		screenStyle = m.styles.warning
	}
	panel = append(panel, m.boxSeparator(details.screenStatus, screenStyle, m.width))
	panel = append(panel, m.previewRows(details, previewRows)...)
	return append(panel, m.boxBottom(m.width))
}

// factValue keeps placeholder words quiet so real facts stand out in the panel.
func (m model) factValue(value string) string {
	switch value {
	case "loading", "unavailable", "unknown":
		return m.styles.muted.Render(value)
	default:
		return value
	}
}

func (m model) previewRows(details inspectionDetails, rows int) []string {
	preview := tailRows(details.preview, rows)
	panel := make([]string, 0, rows)
	for row := range rows {
		line := ""
		if row < len(preview) {
			line = preview[row]
		}
		if !details.previewStyled {
			line = m.styles.command.Render(line)
		}
		panel = append(panel, m.boxRow(line, m.width))
	}
	return panel
}

func (m model) fullPreviewPanel(rows int) []string {
	if rows <= 0 {
		return nil
	}
	_, current, ok := m.currentSession()
	if !ok || rows < 2 {
		return fitRows(nil, rows)
	}
	details := m.detailsFor(current)
	screenTitle := "session screen"
	if m.inspection.beforePicker {
		screenTitle = "screen before picker"
	} else if m.inspection.hasValue && m.inspection.problem != "" {
		screenTitle = "last screen · refresh failed"
	} else if m.inspection.hasValue {
		screenTitle = "current screen"
	}
	panel := make([]string, 0, rows)
	panel = append(panel, m.boxTop(screenTitle+"  ·  "+m.sessionHeadline(current)+"  ·  "+safeText(current.id), m.width))
	panel = append(panel, m.previewRows(details, rows-2)...)
	return append(panel, m.boxBottom(m.width))
}

func tailRows(lines []string, rows int) []string {
	if rows <= 0 {
		return nil
	}
	if len(lines) <= rows {
		return lines
	}
	return lines[len(lines)-rows:]
}

func (m model) previewSubtitle() string {
	_, current, ok := m.currentSession()
	if !ok {
		return "No session selected."
	}
	details := m.detailsFor(current)
	parts := []string{
		m.sessionHeadline(current),
		safeText(current.id),
	}
	if details.screenStatus != "live" {
		parts = append(parts, details.screenStatus)
	}
	parts = append(parts, details.attachment, details.directoryLabel+" "+details.directory)
	return strings.Join(parts, "  ·  ")
}

func (m model) bestSessionLabel(current session) string {
	if summary, ok := m.summaries[inspectionTarget{hostAlias: m.currentHost().alias, sessionID: current.id}]; ok {
		return summarizedSessionLabel(current, summary)
	}
	if m.inspection.kind == inspectionReady && m.inspection.target.sessionID == current.id {
		return inspectedSessionLabel(current, m.inspection.value)
	}
	return sessionLabel(current)
}

// sessionHeadline names a session the way its row does: terminal title first,
// project label when the host has not reported one.
func (m model) sessionHeadline(current session) string {
	title := ""
	if summary, ok := m.summaries[inspectionTarget{hostAlias: m.currentHost().alias, sessionID: current.id}]; ok {
		title = summary.terminalTitle
	}
	if m.inspection.kind == inspectionReady && m.inspection.target.sessionID == current.id {
		title = m.inspection.value.TerminalTitle
	}
	primary, _ := sessionHeadline(m.bestSessionLabel(current), title)
	return primary
}

func outputAge(now time.Time, lastOutput *time.Time) string {
	if lastOutput == nil {
		return "no output observed"
	}
	if !now.After(*lastOutput) || now.Sub(*lastOutput) < time.Second {
		return "output just now"
	}
	return "output " + age(now, *lastOutput) + " ago"
}

func inspectionOutputAge(localNow time.Time, state inspectionState) string {
	observedAt := state.value.ObservedAt
	if observedAt.IsZero() {
		observedAt = localNow
	}
	if !state.receivedAt.IsZero() && localNow.After(state.receivedAt) {
		observedAt = observedAt.Add(localNow.Sub(state.receivedAt))
	}
	return outputAge(observedAt, state.value.LastOutputAt)
}

func (m model) inspectionTargetContainsPicker(target inspectionTarget) bool {
	_, ok := m.containingSessionFor(target)
	return ok
}

func (m model) containingSessionFor(target inspectionTarget) (containingSessionState, bool) {
	host := m.currentHost()
	if host.alias != target.hostAlias || host.id == "" || target.sessionID == "" {
		return containingSessionState{}, false
	}
	state, ok := m.containingSessions[containingSessionKey{hostID: host.id, sessionID: target.sessionID}]
	return state, ok
}

func cloneSessionInspection(source cli.SessionInspection) cli.SessionInspection {
	cloned := source
	cloned.Nested = protocol.CloneSessionIdentities(source.Nested)
	cloned.Preview = append([]string(nil), source.Preview...)
	cloned.StyledPreview = make([]protocol.PreviewLine, len(source.StyledPreview))
	for row, line := range source.StyledPreview {
		cloned.StyledPreview[row].Runs = append([]protocol.PreviewRun(nil), line.Runs...)
	}
	if source.LastOutputAt != nil {
		lastOutputAt := *source.LastOutputAt
		cloned.LastOutputAt = &lastOutputAt
	}
	return cloned
}

func (m model) renderInspectionPreview(inspection cli.SessionInspection) ([]string, bool) {
	lines := make([]string, len(inspection.Preview))
	stylesMatch := len(inspection.StyledPreview) == len(inspection.Preview)
	for row, plain := range inspection.Preview {
		if !stylesMatch || previewLineText(inspection.StyledPreview[row]) != plain {
			for fallbackRow, fallback := range inspection.Preview {
				lines[fallbackRow] = safeText(fallback)
			}
			return lines, false
		}
		var rendered strings.Builder
		for _, run := range inspection.StyledPreview[row].Runs {
			if run.Text == "" {
				continue
			}
			rendered.WriteString(m.previewRunStyle(run.Style).Render(run.Text))
		}
		lines[row] = rendered.String()
	}
	return lines, stylesMatch
}

func previewLineText(line protocol.PreviewLine) string {
	var text strings.Builder
	for _, run := range line.Runs {
		text.WriteString(run.Text)
	}
	return text.String()
}

func (m model) previewRunStyle(presentation protocol.PreviewStyle) lipgloss.Style {
	style := lipgloss.NewStyle().
		Bold(presentation.Bold).
		Faint(presentation.Faint).
		Italic(presentation.Italic).
		Reverse(presentation.Reverse).
		Strikethrough(presentation.Strikethrough)
	if color, ok := previewColor(presentation.Foreground); ok {
		style = style.Foreground(color)
	}
	if color, ok := previewColor(presentation.Background); ok {
		style = style.Background(color)
	}
	if color, ok := previewColor(presentation.UnderlineColor); ok {
		style = style.UnderlineColor(color)
	}
	style = style.UnderlineStyle(previewUnderline(presentation.Underline))
	return style
}

func previewColor(value protocol.PreviewColor) (color.Color, bool) {
	switch value.Kind {
	case protocol.PreviewColorDefault:
		return nil, false
	case protocol.PreviewColorBasic:
		if value.Value > 15 {
			return nil, false
		}
		return ansi.BasicColor(uint8(value.Value)), true
	case protocol.PreviewColorIndexed:
		if value.Value > 255 {
			return nil, false
		}
		return lipgloss.ANSIColor(uint8(value.Value)), true
	case protocol.PreviewColorRGB:
		if value.Value > 0xffffff {
			return nil, false
		}
		return lipgloss.Color(fmt.Sprintf("#%06X", value.Value)), true
	default:
		return nil, false
	}
}

func previewUnderline(value protocol.PreviewUnderline) lipgloss.Underline {
	switch value {
	case protocol.PreviewUnderlineSingle:
		return lipgloss.UnderlineSingle
	case protocol.PreviewUnderlineDouble:
		return lipgloss.UnderlineDouble
	case protocol.PreviewUnderlineCurly:
		return lipgloss.UnderlineCurly
	case protocol.PreviewUnderlineDotted:
		return lipgloss.UnderlineDotted
	case protocol.PreviewUnderlineDashed:
		return lipgloss.UnderlineDashed
	default:
		return lipgloss.UnderlineNone
	}
}

func (m model) boxTop(title string, width int) string {
	return m.boxEdge("┌", "┐", title, m.styles.accent, width)
}

func (m model) boxSeparator(label string, style lipgloss.Style, width int) string {
	return m.boxEdge("├", "┤", label, style, width)
}

func (m model) boxBottom(width int) string {
	return m.boxEdge("└", "┘", "", m.styles.border, width)
}

func (m model) boxEdge(left, right, label string, style lipgloss.Style, width int) string {
	if width < 2 {
		return truncate(label, width)
	}
	inside := width - 2
	if label == "" {
		return m.styles.border.Render(left + strings.Repeat("─", inside) + right)
	}
	label = ansi.Truncate(label, max(0, inside-3), "…")
	fill := strings.Repeat("─", max(0, inside-3-ansi.StringWidth(label)))
	return m.styles.border.Render(left+"─ ") + style.Render(label) + m.styles.border.Render(" "+fill+right)
}

// boxRow frames one content line. The border is styled separately so preview
// colors never bleed into it and the row still pads to the full width.
func (m model) boxRow(content string, width int) string {
	if width < 2 {
		return truncate(content, width)
	}
	return m.styles.border.Render("│") + cell(" "+content, width-2) + m.styles.border.Render("│")
}
