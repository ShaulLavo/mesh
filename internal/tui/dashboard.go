package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/usagefeed"
)

func NewCLIDashboard(output *os.File) cli.DashboardFunc {
	return func(ctx context.Context, input cli.DashboardInput) error {
		if output == nil || !term.IsTerminal(output.Fd()) {
			return errors.New("dashboard needs terminal output")
		}
		return runDashboard(ctx, input, output)
	}
}
func runDashboard(ctx context.Context, input cli.DashboardInput, output io.Writer, options ...tea.ProgramOption) error {
	if input.Theme != "" {
		if err := cli.ValidateDashboardTheme(input.Theme); err != nil {
			return fmt.Errorf("dashboard theme: %w", err)
		}
	}
	if input.Watch == nil {
		return errors.New("dashboard requires a state watch")
	}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	terminal := os.Getenv("TERM")
	model := newDashboardForTerminal(input, time.Now(), terminal)
	model.ctx = run
	configuration := []tea.ProgramOption{tea.WithContext(run), tea.WithInput(nil), tea.WithOutput(output), tea.WithoutSignals()}
	switch terminal {
	case "linux":
		configuration = append(configuration, tea.WithColorProfile(colorprofile.ANSI))
	case "dumb":
		configuration = append(configuration, tea.WithColorProfile(colorprofile.ASCII))
	}
	configuration = append(configuration, options...)
	program := tea.NewProgram(model, configuration...)
	readers := watchDashboard(run, input, program)
	final, err := program.Run()
	cancel()
	readers.Wait()
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("dashboard terminal: %w", err)
	}
	return final.(dashboardModel).watchError
}

func watchDashboard(ctx context.Context, input cli.DashboardInput, program *tea.Program) *sync.WaitGroup {
	readers := new(sync.WaitGroup)
	if input.UsageWatch != nil {
		readers.Go(func() {
			err := input.UsageWatch(ctx, func(result usagefeed.Result) {
				if ctx.Err() == nil {
					program.Send(dashboardUsageMsg(result))
				}
			})
			if err != nil && ctx.Err() == nil {
				program.Send(dashboardUsageMsg{Failing: true})
			}
		})
	}
	readers.Go(func() {
		err := input.Watch(ctx, func(view cli.DashboardHostView) {
			if ctx.Err() == nil {
				program.Send(dashboardHostMsg(view))
			}
		})
		if ctx.Err() == nil {
			program.Send(dashboardDoneMsg{err: err})
		}
	})
	return readers
}

type dashboardUsageMsg usagefeed.Result
type dashboardHostMsg cli.DashboardHostView
type dashboardTickMsg time.Time
type dashboardDoneMsg struct{ err error }
type dashboardRenderWork struct {
	summaries, serviceWidthVisits, layouts, gpuPairs int
}

type dashboardModel struct {
	layout            dashboardLayout
	layoutPrepared    bool
	attentionData     *dashboardAttention
	serviceHostWidth  int
	renderWork        *dashboardRenderWork
	hosts             []cli.DashboardHostView
	now               time.Time
	width, height     int
	watchError        error
	notice            string
	frame             string
	history           map[string]dashboardHostHistory
	ramTotals         map[string]uint64
	wall, ascii       bool
	profile           colorprofile.Profile
	palette           dashboardPalette
	usageEnabled      bool
	usageFailing      bool
	usageRevision     uint64
	usage             dashboardUsage
	ctx               context.Context
	inspect           cli.PickerInspectFunc
	sessionSummaries  map[dashboardSessionTarget]sessionLiveSummary
	inspectionPending bool
	nextInspection    time.Time
}

func newDashboardForTerminal(input cli.DashboardInput, now time.Time, terminal string) dashboardModel {
	model := newDashboard(input, now)
	switch terminal {
	case "linux":
		model.ascii, model.profile = true, colorprofile.ANSI
	case "dumb":
		model.ascii, model.profile = true, colorprofile.ASCII
	}
	return model
}

func newDashboard(input cli.DashboardInput, now time.Time) dashboardModel {
	model := dashboardModel{palette: dashboardTheme(input.Theme), profile: colorprofile.TrueColor, now: now, width: 80, height: 24, wall: input.Wall, history: map[string]dashboardHostHistory{}, ramTotals: map[string]uint64{}}
	for _, host := range input.Hosts {
		model.hosts = append(model.hosts, cli.DashboardHostView{Host: host, Connection: cli.StateConnecting})
	}
	model.notice = safeText(input.Notice)
	model.usageEnabled = input.UsageWatch != nil
	model.ctx, model.inspect = context.Background(), input.Inspect
	model.sessionSummaries = map[dashboardSessionTarget]sessionLiveSummary{}
	model.sortHosts()
	return model
}
func (m dashboardModel) Init() tea.Cmd { return dashboardTick() }
func dashboardTick() tea.Cmd {
	return tea.Tick(time.Second, func(at time.Time) tea.Msg { return dashboardTickMsg(at) })
}
func (m dashboardModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.ColorProfileMsg:
		m.profile = message.Profile
		m.frame, m.layout = m.renderFrame()
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, message.Width), max(1, message.Height)
		m.frame, m.layout = m.renderFrame()
	case dashboardUsageMsg:
		m.usageFailing = message.Failing
		if message.Snapshot != nil && message.Revision != m.usageRevision {
			m.usage = projectDashboardUsage(message.Snapshot)
			m.usageRevision = message.Revision
		}
	case dashboardHostMsg:
		m.receive(cli.DashboardHostView(message))
		m.pruneSessionSummaries()
	case dashboardSessionSummariesMsg:
		m.inspectionPending = false
		m.acceptSessionSummaries(message)
		m.frame, m.layout = m.renderFrame()
	case dashboardTickMsg:
		m.now = time.Time(message)
		m.pruneHistory()
		m.frame, m.layout = m.renderFrame()
		inspection := m.inspectSessions()
		return m, tea.Batch(dashboardTick(), inspection)
	case dashboardDoneMsg:
		m.watchError = message.err
		return m, tea.Quit
	}
	return m, nil
}
func (m *dashboardModel) receive(view cli.DashboardHostView) {
	for index, host := range m.hosts {
		if host.Host.ID == view.Host.ID {
			view.Host = host.Host
			m.hosts[index] = view
			m.remember(view)
			m.rememberRAMTotal(view)
			return
		}
	}
}
func (m dashboardModel) View() tea.View {
	frame := m.frame
	if frame == "" {
		frame = m.render()
	}
	view := tea.NewView(frame)
	view.AltScreen = m.wall
	view.WindowTitle = "Mesh fleet"
	// Terminal defaults cover every cell, including nested SGR resets; Bubble Tea restores them on exit.
	if m.profile != colorprofile.ASCII {
		view.BackgroundColor = m.palette.backgroundValue
		view.ForegroundColor = m.palette.textValue
		if m.profile == colorprofile.ANSI {
			view.BackgroundColor = ansi.BasicColor(m.palette.background.ansi16)
			view.ForegroundColor = ansi.BasicColor(m.palette.text.ansi16)
		}
	}
	return view
}
