// Package closelog keeps connection diagnostics out of interactive terminals.
package closelog

import (
	"fmt"
	"io"
	"log"

	"github.com/charmbracelet/x/term"
)

// Printf uses the configured log destination when it is not a terminal.
func Printf(format string, args ...any) {
	if interactive(log.Writer()) {
		return
	}
	log.Printf(format, args...)
}

// Fprintf writes an attachment diagnostic only to a noninteractive sink.
func Fprintf(sink io.Writer, format string, args ...any) {
	if interactive(sink) {
		return
	}
	_, _ = fmt.Fprintf(sink, format, args...)
}

func interactive(sink io.Writer) bool {
	file, ok := sink.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(file.Fd())
}
