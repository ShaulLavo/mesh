// Package terminal tracks the rendered state of a session's terminal.
package terminal

import (
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/x/vt"
)

const (
	enterAltScreen = "\x1b[?1049h"
	leaveAltScreen = "\x1b[?1049l"
	clearScreen    = "\x1b[2J\x1b[H"
	resetStyle     = "\x1b[0m"
	showCursor     = "\x1b[?25h"
	hideCursor     = "\x1b[?25l"
)

// Screen consumes PTY output and can render the current terminal state for a
// newly attached client.
type Screen interface {
	io.Writer
	Resize(cols, rows int)
	Snapshot() []byte
}

type emulatorScreen struct {
	mu       sync.Mutex
	emulator *vt.Emulator

	cursorVisible bool
	cursorStyle   vt.CursorStyle
	cursorSteady  bool
}

// NewScreen returns an isolated wrapper around x/vt. Keeping x/vt behind this
// interface contains its experimental API within this package.
func NewScreen(cols, rows int) Screen {
	s := &emulatorScreen{
		emulator:      vt.NewEmulator(cols, rows),
		cursorVisible: true,
		cursorStyle:   vt.CursorBlock,
	}
	s.emulator.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) {
			s.cursorVisible = visible
		},
		CursorStyle: func(style vt.CursorStyle, steady bool) {
			// The pinned x/vt callback passes the cursor's steady flag even
			// though the argument is currently documented as blink.
			s.cursorStyle = style
			s.cursorSteady = steady
		},
	})
	return s
}

func (s *emulatorScreen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emulator.Write(p)
}

func (s *emulatorScreen) Resize(cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emulator.Resize(cols, rows)
}

// Snapshot returns the escape sequence stream needed to recreate the active
// screen, cursor, and cell attributes on a fresh terminal.
func (s *emulatorScreen) Snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	var snapshot strings.Builder
	snapshot.WriteString(resetStyle)
	snapshot.WriteString(hideCursor)
	if s.emulator.IsAltScreen() {
		snapshot.WriteString(enterAltScreen)
	} else {
		snapshot.WriteString(leaveAltScreen)
	}
	snapshot.WriteString(clearScreen)
	snapshot.WriteString(s.emulator.Render())
	snapshot.WriteString(resetStyle)

	position := s.emulator.CursorPosition()
	snapshot.WriteString("\x1b[")
	snapshot.WriteString(strconv.Itoa(position.Y + 1))
	snapshot.WriteByte(';')
	snapshot.WriteString(strconv.Itoa(position.X + 1))
	snapshot.WriteByte('H')
	snapshot.WriteString(cursorStyleSequence(s.cursorStyle, s.cursorSteady))
	if s.cursorVisible {
		snapshot.WriteString(showCursor)
	} else {
		snapshot.WriteString(hideCursor)
	}

	return []byte(snapshot.String())
}

func cursorStyleSequence(style vt.CursorStyle, steady bool) string {
	code := 1
	switch style {
	case vt.CursorBlock:
		code = 1
	case vt.CursorUnderline:
		code = 3
	case vt.CursorBar:
		code = 5
	}
	if steady {
		code++
	}
	return "\x1b[" + strconv.Itoa(code) + " q"
}

var _ Screen = (*emulatorScreen)(nil)
