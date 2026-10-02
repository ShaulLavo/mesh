package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
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
	configuration := []tea.ProgramOption{tea.WithContext(run), tea.WithInput(nil), tea.WithOutput(output), tea.WithoutSignals()}
	switch terminal {
	case "linux":
		configuration = append(configuration, tea.WithColorProfile(colorprofile.ANSI))
	case "dumb":
		configuration = append(configuration, tea.WithColorProfile(colorprofile.ASCII))
	}
	configuration = append(configuration, options...)
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
	frame         string
	history       map[string]dashboardHostHistory
	capacity      map[string]uint64
	wall, ascii   bool
	profile       colorprofile.Profile
	palette       dashboardPalette
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
	model := dashboardModel{palette: dashboardTheme(input.Theme), profile: colorprofile.TrueColor, now: now, width: 80, height: 24, wall: input.Wall, history: map[string]dashboardHostHistory{}, capacity: map[string]uint64{}}
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
	case tea.ColorProfileMsg:
		m.profile = message.Profile
		m.frame = m.render()
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, message.Width), max(1, message.Height)
		m.frame = m.render()
	case dashboardHostMsg:
		m.receive(cli.DashboardHostView(message))
	case dashboardTickMsg:
		m.now = time.Time(message)
		m.pruneHistory()
		m.frame = m.render()
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
			m.orderByCapacity(view)
			return
		}
	}
}

// orderByCapacity puts the biggest machines first, since they usually carry
// the most work. RAM total is fixed per machine, so remembering the largest
// seen keeps the order stable when a host drops offline and loses its
// measurement. Equal machines fall back to alias so arrival order can't swap
// them; hosts never measured sort last.
func (m *dashboardModel) orderByCapacity(view cli.DashboardHostView) {
	total := view.RAM.Value.TotalBytes
	if total <= m.capacity[view.Host.ID] {
		return
	}
	m.capacity[view.Host.ID] = total
	sort.SliceStable(m.hosts, func(a, b int) bool {
		left, right := m.capacity[m.hosts[a].Host.ID], m.capacity[m.hosts[b].Host.ID]
		if left != right {
			return left > right
		}
		return m.hosts[a].Host.Alias < m.hosts[b].Host.Alias
	})
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
