// Package terminal tracks the rendered state of a session's terminal.
package terminal

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"sync"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	ansiparser "github.com/charmbracelet/x/ansi/parser"
	"github.com/charmbracelet/x/vt"
)

const (
	enterAltScreen = "\x1b[?1049h"
	leaveAltScreen = "\x1b[?1049l"
	clearScreen    = "\x1b[2J\x1b[H"
	resetStyle     = "\x1b[0m"
	showCursor     = "\x1b[?25h"
	hideCursor     = "\x1b[?25l"

	// x/vt bounds its own string-sequence data at 4 MiB. Match that limit so a
	// peer cannot make the restorable parser tail grow without bound.
	maxParserTail = 4 << 20
)

// Capture is a rendered screen plus any incomplete terminal sequence needed
// to continue parsing future PTY bytes at the same boundary.
type Capture struct {
	Bytes      []byte
	Restorable bool
}

// Screen consumes PTY output and can render the current terminal state for a
// newly attached client.
type Screen interface {
	io.Writer
	Resize(cols, rows int)
	Snapshot() Capture
}

type emulatorScreen struct {
	mu       sync.Mutex
	emulator *vt.Emulator
	parser   *ansi.Parser

	parserTail     []byte
	tailOverflow   bool
	tailHasExecute bool
	linkKnown      bool
	cursors        [2]cursorScreen
	activeScreen   int
	leftRightMode  bool
}

type cursorPresentation struct {
	visible bool
	style   vt.CursorStyle
	steady  bool
	pen     uv.Style
	link    uv.Link
}

type cursorScreen struct {
	current cursorPresentation
	saved   cursorPresentation
}

// NewScreen returns an isolated wrapper around x/vt. Keeping x/vt behind this
// interface contains its experimental API within this package.
func NewScreen(cols, rows int) Screen {
	s := &emulatorScreen{
		emulator: vt.NewEmulator(cols, rows),
		parser:   ansi.NewParser(),
	}
	s.resetCursorState()
	// Cursor state and parser state are private in x/vt. Mirror the small
	// presentation subset needed by Snapshot, while x/vt remains authoritative
	// for cells and cursor position.
	s.parser.SetDataSize(1)
	s.parser.SetHandler(ansi.Handler{
		HandleEsc: s.handleEsc,
		HandleCsi: s.handleCSI,
		HandleOsc: s.handleOSC,
	})
	return s
}

func (s *emulatorScreen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, err := s.emulator.Write(p)
	for _, b := range p[:n] {
		s.advanceParser(b)
	}
	return n, err
}

func (s *emulatorScreen) Resize(cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emulator.Resize(cols, rows)
}

// Snapshot returns the escape sequence stream needed to recreate the active
// screen, cursor, cell attributes, and incremental parser state on a fresh
// terminal.
func (s *emulatorScreen) Snapshot() Capture {
	s.mu.Lock()
	defer s.mu.Unlock()

	var snapshot strings.Builder
	if s.emulator.IsAltScreen() {
		snapshot.WriteString(enterAltScreen)
	} else {
		snapshot.WriteString(leaveAltScreen)
	}
	snapshot.WriteString(resetStyle)
	snapshot.WriteString(ansi.ResetHyperlink())
	snapshot.WriteString(hideCursor)
	snapshot.WriteString(clearScreen)
	// ultraviolet separates physical rows with LF. LNM is reset by default, so
	// LF alone preserves the current column and shifts every later row right.
	snapshot.WriteString(strings.ReplaceAll(s.emulator.Render(), "\n", "\r\n"))
	snapshot.WriteString(resetStyle)

	position := s.emulator.CursorPosition()
	snapshot.WriteString("\x1b[")
	snapshot.WriteString(strconv.Itoa(position.Y + 1))
	snapshot.WriteByte(';')
	snapshot.WriteString(strconv.Itoa(position.X + 1))
	snapshot.WriteByte('H')
	cursor := s.cursors[s.activeScreen].current
	snapshot.WriteString(cursorStyleSequence(cursor.style, cursor.steady))
	if cursor.visible {
		snapshot.WriteString(showCursor)
	} else {
		snapshot.WriteString(hideCursor)
	}
	// Render restores cell attributes, but future text uses the cursor's pen
	// and hyperlink. Put those back after the repaint and before any incomplete
	// parser tail resumes.
	snapshot.WriteString(cursor.pen.String())
	snapshot.WriteString(ansi.SetHyperlink(cursor.link.URL, cursor.link.Params))

	contents := []byte(snapshot.String())
	contents = append(contents, s.parserTail...)
	return Capture{Bytes: contents, Restorable: !s.tailOverflow && !s.tailHasExecute && s.linkKnown}
}

func (s *emulatorScreen) advanceParser(b byte) {
	previous := s.parser.State()
	action := s.parser.Advance(b)
	state := s.parser.State()

	if state == ansiparser.GroundState {
		s.parserTail = s.parserTail[:0]
		s.tailOverflow = false
		s.tailHasExecute = false
		return
	}

	// A parser transition that starts a replacement sequence cancels whatever
	// was pending. Match the transition action: the same byte values are plain
	// payload inside OSC/DCS strings and UTF-8 continuations.
	if startsReplacementSequence(b, action, state) {
		s.parserTail = s.parserTail[:0]
		s.tailOverflow = false
		s.tailHasExecute = false
	} else if previous == ansiparser.GroundState {
		s.parserTail = s.parserTail[:0]
		s.tailOverflow = false
		s.tailHasExecute = false
	}
	// x/ansi treats a second ESC while already in EscapeState as an Execute
	// action even though Advance reports the table's ClearAction. It mutates
	// private command state and therefore has the same non-replayable boundary
	// as any other embedded Execute.
	if previous == ansiparser.EscapeState && b == ansi.ESC {
		s.tailHasExecute = true
	}

	// Execute actions inside CSI/ESC both affect the rendered state and mutate
	// parser command state. Replaying them would duplicate the visible effect,
	// while omitting them can change how the eventual final byte dispatches.
	// There is no faithful byte-stream representation of that boundary.
	if action == ansiparser.ExecuteAction {
		s.tailHasExecute = true
		return
	}
	// Ignore actions do not affect parser state and are safe to omit.
	if action == ansiparser.IgnoreAction {
		return
	}
	if s.tailOverflow {
		return
	}
	if len(s.parserTail) == maxParserTail {
		s.parserTail = s.parserTail[:0]
		s.tailOverflow = true
		return
	}
	s.parserTail = append(s.parserTail, b)
}

func startsReplacementSequence(b byte, action ansiparser.Action, state ansiparser.State) bool {
	if b == ansi.ESC {
		return state == ansiparser.EscapeState
	}
	if !isC1SequenceIntroducer(b) {
		return false
	}
	return action == ansiparser.ClearAction || action == ansiparser.StartAction
}

func isC1SequenceIntroducer(b byte) bool {
	switch b {
	case ansi.CSI, ansi.DCS, ansi.OSC, ansi.SOS, ansi.PM, ansi.APC:
		return true
	default:
		return false
	}
}

func (s *emulatorScreen) handleEsc(cmd ansi.Cmd) {
	if cmd.Prefix() != 0 || cmd.Intermediate() != 0 {
		return
	}
	switch cmd.Final() {
	case '7':
		s.saveCursor()
	case '8':
		s.restoreCursor()
	case 'c':
		s.resetCursorState()
	}
}

func (s *emulatorScreen) handleCSI(cmd ansi.Cmd, params ansi.Params) {
	if cmd.Prefix() == 0 && cmd.Intermediate() == 0 && cmd.Final() == 'm' {
		uv.ReadStyle(params, &s.cursors[s.activeScreen].current.pen)
		return
	}
	if cmd.Prefix() == '?' && cmd.Intermediate() == 0 && (cmd.Final() == 'h' || cmd.Final() == 'l') {
		set := cmd.Final() == 'h'
		for _, packed := range params {
			s.handleMode(packed.Param(-1), set)
		}
		return
	}
	if cmd.Prefix() == 0 && cmd.Intermediate() == ' ' && cmd.Final() == 'q' {
		s.setCursorStyle(params)
		return
	}
	if cmd.Prefix() == 0 && cmd.Intermediate() == 0 && cmd.Final() == 's' && !s.leftRightMode {
		s.saveCursor()
	}
}

func (s *emulatorScreen) handleOSC(_ int, _ []byte) {
	if s.tailOverflow {
		// The x/vt parser retains more OSC data than this mirror. Until a later
		// complete hyperlink command or RIS makes the state known again, do not
		// claim a byte stream can faithfully restore it.
		s.linkKnown = false
		return
	}

	data := s.parserTail
	switch {
	case len(data) >= 2 && data[0] == ansi.ESC && data[1] == ']':
		data = data[2:]
	case len(data) >= 1 && data[0] == ansi.OSC:
		data = data[1:]
	default:
		return
	}
	parts := bytes.Split(data, []byte{';'})
	if len(parts) != 3 {
		return
	}
	if parseOSCCommand(parts[0]) != 8 {
		return
	}
	cursor := &s.cursors[s.activeScreen].current
	cursor.link.Params = string(parts[1])
	cursor.link.URL = string(parts[2])
	s.linkKnown = true
}

func parseOSCCommand(data []byte) int {
	command := -1
	for _, b := range data {
		if b < '0' || b > '9' {
			break
		}
		if command == -1 {
			command = 0
		}
		command = command*10 + int(b-'0')
	}
	return command
}

func (s *emulatorScreen) handleMode(mode int, set bool) {
	switch mode {
	case 25:
		cursor := &s.cursors[s.activeScreen].current
		cursor.visible = set
	case 69:
		s.leftRightMode = set
	case 1047:
		s.setAltScreen(set)
	case 1048:
		if set {
			s.saveCursor()
		} else {
			s.restoreCursor()
		}
	case 1049:
		if set {
			s.saveCursor()
		}
		s.setAltScreen(set)
	}
}

func (s *emulatorScreen) setCursorStyle(params ansi.Params) {
	n := 1
	if param, _, ok := params.Param(0, 0); ok && param > n {
		n = param
	}
	blink := n == 0 || n%2 == 1
	style := n / 2
	if !blink {
		style--
	}
	cursor := &s.cursors[s.activeScreen].current
	cursor.style = vt.CursorStyle(style)
	cursor.steady = !blink
}

func (s *emulatorScreen) setAltScreen(on bool) {
	if on {
		if s.activeScreen == 1 {
			return
		}
		s.cursors[1].current = s.cursors[0].current
		s.activeScreen = 1
		return
	}
	if s.activeScreen == 0 {
		return
	}
	s.activeScreen = 0
}

func (s *emulatorScreen) saveCursor() {
	s.cursors[s.activeScreen].saved = s.cursors[s.activeScreen].current
}

func (s *emulatorScreen) restoreCursor() {
	s.cursors[s.activeScreen].current = s.cursors[s.activeScreen].saved
}

func (s *emulatorScreen) resetCursorState() {
	initial := cursorPresentation{visible: true, style: vt.CursorBlock}
	s.cursors = [2]cursorScreen{
		{current: initial, saved: initial},
		{current: initial, saved: initial},
	}
	s.activeScreen = 0
	s.leftRightMode = false
	s.linkKnown = true
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
