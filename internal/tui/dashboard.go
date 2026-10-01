package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"

	"github.com/shaul/mesh/internal/cli"
)

func NewCLIDashboard(input, output *os.File) cli.DashboardFunc {
	return func(ctx context.Context, catalog cli.DashboardInput) error {
		if output == nil || !term.IsTerminal(output.Fd()) {
			return errors.New("dashboard needs terminal output")
		}
		var reader io.Reader
		if input != nil && term.IsTerminal(input.Fd()) {
			reader = input
		}
		return runDashboard(ctx, catalog, reader, output)
	}
}

func runDashboard(ctx context.Context, catalog cli.DashboardInput, input io.Reader, output io.Writer, options ...tea.ProgramOption) error {
	watchContext, cancel := context.WithCancel(ctx)
	defer cancel()
	configuration := []tea.ProgramOption{tea.WithContext(watchContext), tea.WithInput(input), tea.WithOutput(output), tea.WithoutSignals()}
	configuration = append(configuration, options...)
	program := tea.NewProgram(newDashboard(catalog, time.Now(), os.Getenv("TERM") == "linux"), configuration...)
	joined := make(chan struct{})
	go dashboardWatch(watchContext, catalog.Watch, program.Send, joined)
	final, err := program.Run()
	cancel()
	<-joined
	if err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}
	return final.(dashboardModel).watchError
}

func dashboardWatch(ctx context.Context, watch cli.DashboardWatch, send func(tea.Msg), joined chan<- struct{}) {
	defer close(joined)
	if watch == nil {
		return
	}
	err := watch(ctx, func(view cli.DashboardHostView) {
		if ctx.Err() == nil {
			send(dashboardHostMsg(cloneDashboardView(view)))
		}
	})
	if ctx.Err() == nil {
		send(dashboardWatchDoneMsg{err: err})
	}
}

func cloneDashboardView(view cli.DashboardHostView) cli.DashboardHostView {
	view.Sessions = append([]cli.DashboardSession(nil), view.Sessions...)
	view.Services = append([]cli.DashboardService(nil), view.Services...)
	for index := range view.Sessions {
		if view.Sessions[index].LastOutputAt != nil {
			copied := *view.Sessions[index].LastOutputAt
			view.Sessions[index].LastOutputAt = &copied
		}
	}
	return view
}

type dashboardHostMsg cli.DashboardHostView
type dashboardTickMsg time.Time
type dashboardWatchDoneMsg struct{ err error }

type dashboardModel struct {
	hosts      []cli.DashboardHostView
	history    map[string]dashboardHostHistory
	now        time.Time
	width      int
	height     int
	ascii      bool
	watchError error
}

func newDashboard(input cli.DashboardInput, now time.Time, ascii bool) dashboardModel {
	current := dashboardModel{now: now, width: 80, height: 24, ascii: ascii, history: make(map[string]dashboardHostHistory)}
	for _, host := range input.Hosts {
		current.hosts = append(current.hosts, cli.DashboardHostView{Host: host, Reachability: cli.DashboardConnecting})
	}
	return current
}

func (m dashboardModel) Init() tea.Cmd { return dashboardTick() }

func dashboardTick() tea.Cmd {
	return tea.Tick(time.Second, func(at time.Time) tea.Msg { return dashboardTickMsg(at) })
}

func (m dashboardModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, message.Width), max(1, message.Height)
	case dashboardTickMsg:
		m.now = time.Time(message)
		m.pruneHistory()
		return m, dashboardTick()
	case dashboardHostMsg:
		m.receive(cli.DashboardHostView(message))
	case dashboardWatchDoneMsg:
		m.watchError = message.err
		return m, tea.Quit
	case tea.KeyPressMsg:
		if message.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *dashboardModel) receive(view cli.DashboardHostView) {
	for index := range m.hosts {
		if m.hosts[index].Host.ID != view.Host.ID {
			continue
		}
		view.Host = m.hosts[index].Host
		m.hosts[index] = cloneDashboardView(view)
		m.remember(view)
		return
	}
}

func (m dashboardModel) View() tea.View {
	content := m.render()
	view := tea.NewView(content)
	view.AltScreen = true
	view.WindowTitle = "Mesh fleet"
	view.BackgroundColor = lipgloss.Color("#0c0b0c")
	view.ForegroundColor = lipgloss.Color("#FAFCFB")
	return view
}
