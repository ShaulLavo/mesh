package cli

import (
	"fmt"
	"io"
	"os"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/fang"
	"github.com/charmbracelet/x/term"
)

// RenderStep prints one tagged progress line. The label is styled like the
// ERROR badge, so every prefixed line the CLI prints reads as the same kind of
// thing. Progress runs outside fang.Execute and has no Styles, so the colour
// scheme is built here.
func RenderStep(output io.Writer, label, step, detail string) {
	_, _ = fmt.Fprintf(output, "%s %-9s %s\n", tag(output, label), SafeTerminalText(step), SafeTerminalText(detail))
}

// tag styles for a terminal and returns the bare label otherwise. It asks the
// writer it is about to print to, not a global, so a redirected or captured
// stream never gets escape sequences.
func tag(output io.Writer, label string) string {
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
