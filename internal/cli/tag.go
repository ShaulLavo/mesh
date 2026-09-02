package cli

import (
	"io"
	"os"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/fang"
	"github.com/charmbracelet/x/term"
)

// tag styles for a terminal and returns the bare label otherwise. It asks the
// writer it is about to print to, not a global, so a redirected or captured
// stream never gets escape sequences.
func Tag(output io.Writer, label string) string {
	file, ok := output.(term.File)
	if !ok || !term.IsTerminal(file.Fd()) {
		return label
	}
	scheme := fang.DefaultColorScheme(lipgloss.LightDark(lipgloss.HasDarkBackground(os.Stdin, os.Stdout)))
	return lipgloss.NewStyle().
		Bold(true).
		Foreground(scheme.Base).
		Background(scheme.Command).
		Padding(0, 1).
		MarginLeft(2).
		Render(label)
}
