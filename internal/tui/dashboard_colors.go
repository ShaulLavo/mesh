package tui

import (
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

type dashboardStyle uint8

const (
	dashboardTextStyle dashboardStyle = iota
	dashboardGPUStyle
	dashboardCPUStyle
	dashboardCPUFillStyle
	dashboardRAMFillStyle
	dashboardGridStyle
	dashboardRAMStyle
	dashboardMutedStyle
	dashboardBorderStyle
	dashboardGoodStyle
	dashboardCachedStyle
	dashboardFailureStyle
	dashboardTitleStyle
)

func (m dashboardModel) paint(role dashboardStyle) lipgloss.Style {
	if m.profile == colorprofile.ASCII {
		return lipgloss.NewStyle()
	}
	palette := m.palette
	colors := [...]dashboardColor{palette.text, palette.gpu, palette.cpu, palette.cpuFill, palette.ramFill, palette.grid, palette.ram, palette.muted, palette.border, palette.good, palette.cached, palette.failure, palette.title}
	value := colors[role]
	foreground := lipgloss.Color(value.hex)
	if m.profile == colorprofile.ANSI {
		// Semantic slots stay distinct even when a theme shares title and RAM hues.
		foreground = ansi.BasicColor(value.ansi16)
	}
	style := lipgloss.NewStyle().Foreground(foreground).Bold(role == dashboardTitleStyle)
	if m.profile == colorprofile.ANSI && (role == dashboardCPUFillStyle || role == dashboardRAMFillStyle) {
		style = style.Faint(true)
	}
	return style
}
