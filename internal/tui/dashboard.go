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

func NewCLIDashboard(output *os.File) cli.DashboardFunc {
	return func(ctx context.Context, input cli.DashboardInput) error {
		if output == nil || !term.IsTerminal(output.Fd()) {
			return errors.New("dashboard needs terminal output")
		}
		return runDashboard(ctx, input, output)
	}
}
func runDashboard(ctx context.Context, input cli.DashboardInput, output io.Writer, options ...tea.ProgramOption) error {
	if input.Watch == nil {
		return errors.New("dashboard requires a state watch")
	}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	configuration := []tea.ProgramOption{tea.WithContext(run), tea.WithInput(nil), tea.WithOutput(output), tea.WithoutSignals()}
	configuration = append(configuration, options...)
	model := newDashboard(input, time.Now())
	model.ascii = os.Getenv("TERM") == "linux" || os.Getenv("TERM") == "dumb"
	program := tea.NewProgram(model, configuration...)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		err := input.Watch(run, func(view cli.DashboardHostView) {
			if run.Err() == nil {
				program.Send(dashboardHostMsg(view))
			}
		})
		if run.Err() == nil {
			program.Send(dashboardDoneMsg{err: err})
		}
	}()
	final, err := program.Run()
	cancel()
	<-joined
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("dashboard terminal: %w", err)
	}
	return final.(dashboardModel).watchError
}

type dashboardHostMsg cli.DashboardHostView
type dashboardTickMsg time.Time
type dashboardDoneMsg struct{ err error }
type dashboardModel struct {
	hosts         []cli.DashboardHostView
	now           time.Time
	width, height int
	watchError    error
	history       map[string]dashboardHostHistory
	wall, ascii   bool
}

func newDashboard(input cli.DashboardInput, now time.Time) dashboardModel {
	model := dashboardModel{now: now, width: 80, height: 24, wall: input.Wall, history: map[string]dashboardHostHistory{}}
	for _, host := range input.Hosts {
		model.hosts = append(model.hosts, cli.DashboardHostView{Host: host, Connection: cli.StateConnecting})
	}
	return model
}
func (m dashboardModel) Init() tea.Cmd { return dashboardTick() }
func dashboardTick() tea.Cmd {
	return tea.Tick(time.Second, func(at time.Time) tea.Msg { return dashboardTickMsg(at) })
}
func (m dashboardModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, message.Width), max(1, message.Height)
	case dashboardHostMsg:
		m.receive(cli.DashboardHostView(message))
	case dashboardTickMsg:
		m.now = time.Time(message)
		m.pruneHistory()
		return m, dashboardTick()
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
			return
		}
	}
}
func (m dashboardModel) View() tea.View {
	view := tea.NewView(m.render())
	view.AltScreen = m.wall
	view.WindowTitle = "Mesh fleet"
	return view
}
