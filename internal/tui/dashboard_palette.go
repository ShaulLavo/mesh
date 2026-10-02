package tui

import "charm.land/lipgloss/v2"

const (
	dashboardCurrentGreen  = "#7bd88f"
	dashboardRoseIris      = "#c4a7e7"
	dashboardRoseFoam      = "#9ccfd8"
	dashboardOLEDGreen     = "#4dff91"
	dashboardKanagawaGreen = "#98bb6c"
	dashboardGruvboxGreen  = "#a9b665"
)

type dashboardColor struct {
	hex    string
	ansi16 int
}
type dashboardPalette struct {
	name       string
	background dashboardColor
	text       dashboardColor
	muted      dashboardColor
	border     dashboardColor
	grid       dashboardColor
	title      dashboardColor
	cpu        dashboardColor
	cpuFill    dashboardColor
	ram        dashboardColor
	ramFill    dashboardColor
	gpu        dashboardColor
	battery    dashboardColor
	good       dashboardColor
	cached     dashboardColor
	failure    dashboardColor
}

// Palettes keep semantic console slots stable; selecting a theme changes one value.
var dashboardPalettes = []dashboardPalette{
	{name: "Current",
		background: dashboardColor{"#090c0b", 0},
		text:       dashboardColor{"#cfdbd4", 7},
		muted:      dashboardColor{"#7a8f86", 8},
		border:     dashboardColor{"#24453a", 8},
		grid:       dashboardColor{"#17261f", 8},
		title:      dashboardColor{"#5fd7d7", 14},
		cpu:        dashboardColor{dashboardCurrentGreen, 2},
		cpuFill:    dashboardColor{"#183c27", 2},
		ram:        dashboardColor{"#56c8e8", 6},
		ramFill:    dashboardColor{"#153d49", 6},
		gpu:        dashboardColor{"#b39df3", 5},
		battery:    dashboardColor{dashboardCurrentGreen, 2},
		good:       dashboardColor{dashboardCurrentGreen, 2},
		cached:     dashboardColor{"#e0b050", 3},
		failure:    dashboardColor{"#ef6b5b", 1},
	},
	{name: "Rosé Pine",
		background: dashboardColor{"#191724", 0},
		text:       dashboardColor{"#e0def4", 7},
		muted:      dashboardColor{"#908caa", 8},
		border:     dashboardColor{"#403d52", 8},
		grid:       dashboardColor{"#26233a", 8},
		title:      dashboardColor{dashboardRoseIris, 14},
		cpu:        dashboardColor{dashboardRoseFoam, 2},
		cpuFill:    dashboardColor{"#363f4c", 2},
		ram:        dashboardColor{dashboardRoseIris, 6},
		ramFill:    dashboardColor{"#3f374f", 6},
		gpu:        dashboardColor{"#ebbcba", 5},
		battery:    dashboardColor{dashboardRoseFoam, 2},
		good:       dashboardColor{dashboardRoseFoam, 2},
		cached:     dashboardColor{"#f6c177", 3},
		failure:    dashboardColor{"#eb6f92", 1},
	},
	{name: "Rosé Pine Moon",
		background: dashboardColor{"#232136", 0},
		text:       dashboardColor{"#e0def4", 7},
		muted:      dashboardColor{"#908caa", 8},
		border:     dashboardColor{"#44415a", 8},
		grid:       dashboardColor{"#2a283e", 8},
		title:      dashboardColor{dashboardRoseIris, 14},
		cpu:        dashboardColor{dashboardRoseFoam, 2},
		cpuFill:    dashboardColor{"#3e475a", 2},
		ram:        dashboardColor{dashboardRoseIris, 6},
		ramFill:    dashboardColor{"#463e5d", 6},
		gpu:        dashboardColor{"#ea9a97", 5},
		battery:    dashboardColor{dashboardRoseFoam, 2},
		good:       dashboardColor{dashboardRoseFoam, 2},
		cached:     dashboardColor{"#f6c177", 3},
		failure:    dashboardColor{"#eb6f92", 1},
	},
	{name: "OLED high contrast",
		background: dashboardColor{"#000000", 0},
		text:       dashboardColor{"#f2f7f4", 7},
		muted:      dashboardColor{"#9aaba3", 8},
		border:     dashboardColor{"#2c5244", 8},
		grid:       dashboardColor{"#1b2c25", 8},
		title:      dashboardColor{"#3cf2f2", 14},
		cpu:        dashboardColor{dashboardOLEDGreen, 2},
		cpuFill:    dashboardColor{"#0a2e18", 2},
		ram:        dashboardColor{"#33d1ff", 6},
		ramFill:    dashboardColor{"#062a36", 6},
		gpu:        dashboardColor{"#c88cff", 5},
		battery:    dashboardColor{dashboardOLEDGreen, 2},
		good:       dashboardColor{dashboardOLEDGreen, 2},
		cached:     dashboardColor{"#ffc23d", 3},
		failure:    dashboardColor{"#ff5a52", 1},
	},
	{name: "Kanagawa Wave",
		background: dashboardColor{"#1f1f28", 0},
		text:       dashboardColor{"#dcd7ba", 7},
		muted:      dashboardColor{"#938aa9", 8},
		border:     dashboardColor{"#363646", 8},
		grid:       dashboardColor{"#2a2a37", 8},
		title:      dashboardColor{"#7e9cd8", 14},
		cpu:        dashboardColor{dashboardKanagawaGreen, 2},
		cpuFill:    dashboardColor{"#3a4137", 2},
		ram:        dashboardColor{"#7fb4ca", 6},
		ramFill:    dashboardColor{"#34404c", 6},
		gpu:        dashboardColor{"#957fb8", 5},
		battery:    dashboardColor{dashboardKanagawaGreen, 2},
		good:       dashboardColor{dashboardKanagawaGreen, 2},
		cached:     dashboardColor{"#e6c384", 3},
		failure:    dashboardColor{"#ff5d62", 1},
	},
	{name: "Gruvbox Material",
		background: dashboardColor{"#1d2021", 0},
		text:       dashboardColor{"#d4be98", 7},
		muted:      dashboardColor{"#a89984", 8},
		border:     dashboardColor{"#3c3836", 8},
		grid:       dashboardColor{"#282828", 8},
		title:      dashboardColor{"#89b482", 14},
		cpu:        dashboardColor{dashboardGruvboxGreen, 2},
		cpuFill:    dashboardColor{"#3c4130", 2},
		ram:        dashboardColor{"#7daea3", 6},
		ramFill:    dashboardColor{"#323f3e", 6},
		gpu:        dashboardColor{"#d3869b", 5},
		battery:    dashboardColor{dashboardGruvboxGreen, 2},
		good:       dashboardColor{dashboardGruvboxGreen, 2},
		cached:     dashboardColor{"#d8a657", 3},
		failure:    dashboardColor{"#ea6962", 1},
	},
}

var dashboardTheme = dashboardPalettes[0]

func dashboardColorStyle(color dashboardColor) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color.hex))
}
