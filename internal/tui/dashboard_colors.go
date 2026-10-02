package tui

import (
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

func (m dashboardModel) paint(style lipgloss.Style) lipgloss.Style {
	return dashboardProfileStyle(style, m.profile)
}

// Select basic colors before rendering so cached yellow cannot quantize to failure red.
func dashboardProfileStyle(style lipgloss.Style, profile colorprofile.Profile) lipgloss.Style {
	if profile != colorprofile.ANSI {
		return style
	}
	foreground := lipgloss.White
	switch style.GetForeground() {
	case dashboardCPUStyle.GetForeground():
		foreground = lipgloss.Green
	case dashboardGPUStyle.GetForeground():
		foreground = lipgloss.Magenta
	case dashboardRAMStyle.GetForeground(), dashboardRAMFillStyle.GetForeground():
		foreground = lipgloss.Cyan
	case dashboardCPUFillStyle.GetForeground():
		foreground = lipgloss.Green
	case dashboardCachedStyle.GetForeground():
		foreground = lipgloss.Yellow
	case dashboardFailureStyle.GetForeground():
		foreground = lipgloss.Red
	case dashboardTitleStyle.GetForeground():
		foreground = lipgloss.BrightCyan
	case dashboardMutedStyle.GetForeground(), dashboardBorderStyle.GetForeground(), dashboardGridStyle.GetForeground():
		foreground = lipgloss.BrightBlack
	}
	if style.GetForeground() == dashboardCPUFillStyle.GetForeground() || style.GetForeground() == dashboardRAMFillStyle.GetForeground() {
		style = style.Faint(true)
	}
	style = style.Foreground(foreground)
	if style.GetBackground() == lipgloss.Color(dashboardTheme.background.hex) {
		style = style.Background(lipgloss.Black)
	}
	return style
}

func dashboardGraphStyle(style, paint lipgloss.Style) lipgloss.Style {
	foreground := style.GetForeground()
	if foreground == lipgloss.Green || foreground == lipgloss.Cyan {
		return dashboardProfileStyle(paint, colorprofile.ANSI)
	}
	return paint
}
