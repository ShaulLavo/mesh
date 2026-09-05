// Package terminal tracks the rendered state of a session's terminal.
package terminal

import (
	"bytes"
	"image/color"
	"io"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

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
	maxParserTail       = 4 << 20
	maxOSCMetadataBytes = 4 << 10
	maxPreviewTextBytes = 32 << 10
)

// Capture is a rendered screen plus any incomplete terminal sequence needed
// to continue parsing future PTY bytes at the same boundary.
type Capture struct {
	Bytes      []byte
	Restorable bool
}

// Preview is a bounded view of the active screen and the latest metadata
// reported by the terminal session. StyledLines has the same length as Lines,
// and concatenating each styled line's run text reproduces its plain line.
type Preview struct {
	Lines       []string
	StyledLines []PreviewLine
	Title       string
	Directory   string
}

// PreviewLine contains adjacent terminal cells coalesced by presentation.
type PreviewLine struct {
	Runs []PreviewRun
}

// PreviewRun is control-free text with one terminal presentation.
type PreviewRun struct {
	Text  string
	Style PreviewStyle
}

// PreviewStyle is the non-animated presentation retained for a preview.
// Hyperlinks, blinking, and concealed contents are intentionally omitted.
type PreviewStyle struct {
	Foreground     PreviewColor
	Background     PreviewColor
	UnderlineColor PreviewColor
	Bold           bool
	Faint          bool
	Italic         bool
	Reverse        bool
	Strikethrough  bool
	Underline      PreviewUnderline
}

// PreviewColor is either the default terminal color or a basic, indexed, or
// RGB color. RGB values are packed as 0xRRGGBB.
type PreviewColor struct {
	Kind  PreviewColorKind
	Value uint32
}

// PreviewColorKind identifies how a terminal color should be interpreted.
type PreviewColorKind uint8

const (
	PreviewColorDefault PreviewColorKind = iota
	PreviewColorBasic
	PreviewColorIndexed
	PreviewColorRGB
)

// PreviewUnderline identifies the underline presentation of a cell.
type PreviewUnderline uint8

const (
	PreviewUnderlineNone PreviewUnderline = iota
	PreviewUnderlineSingle
	PreviewUnderlineDouble
	PreviewUnderlineCurly
	PreviewUnderlineDotted
	PreviewUnderlineDashed
)

// Screen consumes PTY output and can render the current terminal state for a
// newly attached client.
type Screen interface {
	io.Writer
	Resize(cols, rows int)
	Snapshot() Capture
	Preview(cols, rows int) Preview
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
	title          string
	directory      string
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
	emulator := vt.NewEmulator(cols, rows)
	// x/vt answers DSR, DA, DECRQM and in-band resize by writing to an internal
	// io.Pipe, which blocks until someone reads it. This screen is a passive
	// shadow renderer for reattachment snapshots, and the attached client's real
	// terminal is what answers those queries, so nobody ever reads that pipe.
	// Closing the writer makes every reply fail instantly with ErrClosedPipe,
	// which x/vt discards, instead of blocking Write forever.
	//
	// Draining it in a goroutine would be the obvious alternative, but Emulator
	// has no mutex and Read, Write and Close all touch its closed flag, so a
	// concurrent drainer races with the pump.
	if pipe, ok := emulator.InputPipe().(io.Closer); ok {
		_ = pipe.Close()
	}
	s := &emulatorScreen{
		emulator: emulator,
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
	if s.title != "" {
		snapshot.WriteString(ansi.SetWindowTitle(s.title))
	}
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

// Preview returns a plain-text crop of the active screen. The crop ends at
// whichever is lower: the cursor or the last row containing visible text.
func (s *emulatorScreen) Preview(cols, rows int) Preview {
	s.mu.Lock()
	defer s.mu.Unlock()

	preview := Preview{
		Title:     strings.Clone(s.title),
		Directory: strings.Clone(s.directory),
	}
	width := min(cols, s.emulator.Width())
	height := s.emulator.Height()
	if width <= 0 || rows <= 0 || height <= 0 {
		return preview
	}

	lastNonblank := -1
	for y := range height {
		if s.rowHasText(y) {
			lastNonblank = y
		}
	}
	cursorRow := min(max(s.emulator.CursorPosition().Y, 0), height-1)
	end := max(cursorRow, lastNonblank)
	start := max(0, end-min(rows, end+1)+1)

	preview.Lines = make([]string, 0, end-start+1)
	preview.StyledLines = make([]PreviewLine, 0, end-start+1)
	remainingBytes := maxPreviewTextBytes
	for y := start; y <= end; y++ {
		line, styledLine, exhausted := s.previewRow(y, width, remainingBytes)
		preview.Lines = append(preview.Lines, line)
		preview.StyledLines = append(preview.StyledLines, styledLine)
		remainingBytes -= len(line)
		if exhausted || remainingBytes == 0 {
			break
		}
	}
	return preview
}

func (s *emulatorScreen) rowHasText(y int) bool {
	for x := range s.emulator.Width() {
		cell := s.emulator.CellAt(x, y)
		if cell == nil {
			continue
		}
		style := previewStyle(cell.Style)
		visibleContent := cell.Style.Attrs&uv.AttrConceal == 0 && cell.Content != "" && cell.Content != " "
		if visibleContent || style != (PreviewStyle{}) {
			return true
		}
	}
	return false
}

func (s *emulatorScreen) previewRow(y, width, byteBudget int) (string, PreviewLine, bool) {
	var line strings.Builder
	line.Grow(min(width, byteBudget))
	styledLine := PreviewLine{}
	var pendingSpaces []PreviewRun
	used := 0
	pendingBytes := 0
	retainedPendingBytes := 0
	for x := 0; x < s.emulator.Width() && used < width; x++ {
		cell := s.emulator.CellAt(x, y)
		if cell == nil || cell.Width <= 0 {
			continue
		}
		if cell.Width > width-used {
			break
		}
		used += cell.Width
		style := previewStyle(cell.Style)
		concealed := cell.Style.Attrs&uv.AttrConceal != 0
		if cell.Content == " " || concealed {
			spaces := cell.Width
			appendPreviewRun(&pendingSpaces, strings.Repeat(" ", spaces), style)
			pendingBytes += spaces
			if concealed || style != (PreviewStyle{}) {
				retainedPendingBytes = pendingBytes
			}
			continue
		}

		available := byteBudget - line.Len() - pendingBytes
		prefixBytes := validUTF8PrefixBytes(cell.Content, available)
		if prefixBytes < len(cell.Content) {
			if prefixBytes > 0 {
				flushPreviewRuns(&line, &styledLine, pendingSpaces)
				appendPreviewText(&line, &styledLine, cell.Content[:prefixBytes], style)
			} else {
				flushPreviewRunPrefix(&line, &styledLine, pendingSpaces, retainedPendingBytes, byteBudget)
			}
			return line.String(), styledLine, true
		}
		flushPreviewRuns(&line, &styledLine, pendingSpaces)
		pendingSpaces = pendingSpaces[:0]
		pendingBytes = 0
		retainedPendingBytes = 0
		appendPreviewText(&line, &styledLine, cell.Content, style)
		if line.Len() == byteBudget {
			return line.String(), styledLine, true
		}
	}
	exhausted := flushPreviewRunPrefix(&line, &styledLine, pendingSpaces, retainedPendingBytes, byteBudget)
	return line.String(), styledLine, exhausted
}

func flushPreviewRuns(line *strings.Builder, styledLine *PreviewLine, runs []PreviewRun) {
	for _, run := range runs {
		appendPreviewText(line, styledLine, run.Text, run.Style)
	}
}

func flushPreviewRunPrefix(
	line *strings.Builder,
	styledLine *PreviewLine,
	runs []PreviewRun,
	prefixBytes int,
	byteBudget int,
) bool {
	for _, run := range runs {
		if prefixBytes == 0 {
			break
		}
		textBytes := min(len(run.Text), prefixBytes, max(0, byteBudget-line.Len()))
		appendPreviewText(line, styledLine, run.Text[:textBytes], run.Style)
		prefixBytes -= textBytes
		if line.Len() == byteBudget {
			return true
		}
	}
	return false
}

func appendPreviewText(line *strings.Builder, styledLine *PreviewLine, text string, style PreviewStyle) {
	line.WriteString(text)
	appendPreviewRun(&styledLine.Runs, text, style)
}

func appendPreviewRun(runs *[]PreviewRun, text string, style PreviewStyle) {
	if text == "" {
		return
	}
	if len(*runs) > 0 && (*runs)[len(*runs)-1].Style == style {
		(*runs)[len(*runs)-1].Text += text
		return
	}
	*runs = append(*runs, PreviewRun{Text: text, Style: style})
}

func previewStyle(style uv.Style) PreviewStyle {
	return PreviewStyle{
		Foreground:     previewColor(style.Fg),
		Background:     previewColor(style.Bg),
		UnderlineColor: previewColor(style.UnderlineColor),
		Bold:           style.Attrs&uv.AttrBold != 0,
		Faint:          style.Attrs&uv.AttrFaint != 0,
		Italic:         style.Attrs&uv.AttrItalic != 0,
		Reverse:        style.Attrs&uv.AttrReverse != 0,
		Strikethrough:  style.Attrs&uv.AttrStrikethrough != 0,
		Underline:      previewUnderline(style.Underline),
	}
}

func previewColor(value color.Color) PreviewColor {
	switch value := value.(type) {
	case nil:
		return PreviewColor{}
	case ansi.BasicColor:
		return PreviewColor{Kind: PreviewColorBasic, Value: uint32(value)}
	case ansi.IndexedColor:
		return PreviewColor{Kind: PreviewColorIndexed, Value: uint32(value)}
	case ansi.RGBColor:
		return PreviewColor{
			Kind:  PreviewColorRGB,
			Value: uint32(value.R)<<16 | uint32(value.G)<<8 | uint32(value.B),
		}
	default:
		r, g, b, _ := value.RGBA()
		return PreviewColor{
			Kind: PreviewColorRGB,
			Value: ((r>>8)&0xff)<<16 |
				((g>>8)&0xff)<<8 |
				((b >> 8) & 0xff),
		}
	}
}

func previewUnderline(value uv.Underline) PreviewUnderline {
	switch value {
	case uv.UnderlineSingle:
		return PreviewUnderlineSingle
	case uv.UnderlineDouble:
		return PreviewUnderlineDouble
	case uv.UnderlineCurly:
		return PreviewUnderlineCurly
	case uv.UnderlineDotted:
		return PreviewUnderlineDotted
	case uv.UnderlineDashed:
		return PreviewUnderlineDashed
	default:
		return PreviewUnderlineNone
	}
}

func validUTF8PrefixBytes(value string, maxBytes int) int {
	limit := min(len(value), max(maxBytes, 0))
	prefixBytes := 0
	for prefixBytes < limit {
		r, size := utf8.DecodeRuneInString(value[prefixBytes:])
		if r == utf8.RuneError && size == 1 || size > limit-prefixBytes {
			break
		}
		prefixBytes += size
	}
	return prefixBytes
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

	if commandData, payload, ok := bytes.Cut(data, []byte{';'}); ok {
		if command, valid := parseExactOSCCommand(commandData); valid {
			switch command {
			case 0, 2:
				if validOSCMetadata(payload) {
					s.title = string(payload)
				}
			case 7:
				if directory, valid := parseOSC7Directory(payload); valid {
					s.directory = directory
				}
			}
		}
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

func parseExactOSCCommand(data []byte) (int, bool) {
	if len(data) == 0 || len(data) > 19 {
		return 0, false
	}
	command := 0
	for _, b := range data {
		if b < '0' || b > '9' {
			return 0, false
		}
		digit := int(b - '0')
		if command > (int(^uint(0)>>1)-digit)/10 {
			return 0, false
		}
		command = command*10 + digit
	}
	return command, true
}

func validOSCMetadata(data []byte) bool {
	if len(data) > maxOSCMetadataBytes || !utf8.Valid(data) {
		return false
	}
	for _, r := range string(data) {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func parseOSC7Directory(data []byte) (string, bool) {
	if !validOSCMetadata(data) {
		return "", false
	}
	location, err := url.Parse(string(data))
	if err != nil || !strings.EqualFold(location.Scheme, "file") || location.Opaque != "" ||
		location.User != nil || location.RawQuery != "" || location.ForceQuery || location.Fragment != "" ||
		!path.IsAbs(location.Path) || !validOSCMetadata([]byte(location.Path)) {
		return "", false
	}
	return strings.Clone(location.Path), true
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
