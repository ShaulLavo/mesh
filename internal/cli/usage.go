package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/fang"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

// usageError says what the command needs and shows an invocation that works.
// Cobra's built-in message counts arguments, which never tells the reader what
// to type instead.
type usageError struct {
	problem string
	example string
}

func (e *usageError) Error() string {
	if e.example == "" {
		return e.problem
	}
	return e.problem + "; try: " + e.example
}

// exactArgs enforces an argument count and explains the fix when it is wrong.
func exactArgs(count int, needs, example string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == count {
			return nil
		}
		return &usageError{
			problem: fmt.Sprintf("%s needs %s", cmd.CommandPath(), needs),
			example: example,
		}
	}
}

// RenderError puts the badge and the message on one line. Fang's default
// prints the badge alone, then the message on the line below it.
func RenderError(output io.Writer, styles fang.Styles, err error) {
	if file, ok := output.(term.File); ok && !term.IsTerminal(file.Fd()) {
		_, _ = fmt.Fprintln(output, err.Error())
		return
	}

	problem, example := err.Error(), ""
	var usage *usageError
	if errors.As(err, &usage) {
		problem, example = usage.problem, usage.example
	}

	// The badge carries Margin(1), which paints blank rows above and below it
	// the moment anything is joined beside it. Keep only the left inset.
	badge := styles.ErrorHeader.Margin(0).MarginLeft(2)
	text := styles.ErrorText.UnsetWidth().UnsetMargins().UnsetTransform()
	indent := strings.Repeat(" ", lipgloss.Width(badge.String()))

	_, _ = fmt.Fprintln(output)
	_, _ = fmt.Fprintln(output, lipgloss.JoinHorizontal(
		lipgloss.Left,
		badge.String(),
		text.Render(" "+problem),
	))
	if example != "" {
		_, _ = fmt.Fprintln(output, text.Render(indent+" try:  "+example))
	}
	_, _ = fmt.Fprintln(output)
}
