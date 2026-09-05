package terminal

import (
	"slices"
	"strings"
	"unicode"

	uv "github.com/charmbracelet/ultraviolet"
)

// TextSnapshot copies bounded cells while the emulator is locked. Render runs
// after the worker releases its PTY lock and never produces terminal controls.
type TextSnapshot struct {
	Title     string
	Directory string
	rows      [][]textCell
}

type textCell struct {
	content string
}

func (s *emulatorScreen) SaveText(maxLines, maxBytes int) TextSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	snapshot := TextSnapshot{Title: s.title, Directory: s.directory}
	if maxLines <= 0 || maxBytes <= 0 {
		return snapshot
	}
	height := s.emulator.Height()
	last := min(max(s.emulator.CursorPosition().Y, 0), height-1)
	for y := height - 1; y > last; y-- {
		if s.rowHasText(y) {
			last = y
			break
		}
	}
	for y := last; y >= 0 && len(snapshot.rows) < maxLines && maxBytes > 0; y-- {
		row := copyTextRow(s.emulator.Width(), func(x int) *uv.Cell { return s.emulator.CellAt(x, y) }, &maxBytes)
		snapshot.rows = append(snapshot.rows, row)
	}
	for y := s.emulator.ScrollbackLen() - 1; y >= 0 && len(snapshot.rows) < maxLines && maxBytes > 0; y-- {
		line := s.emulator.Scrollback().Line(y)
		row := copyTextRow(len(line), func(x int) *uv.Cell { return &line[x] }, &maxBytes)
		snapshot.rows = append(snapshot.rows, row)
	}
	slices.Reverse(snapshot.rows)
	return snapshot
}

func copyTextRow(width int, at func(int) *uv.Cell, remaining *int) []textCell {
	row := make([]textCell, 0, min(width, *remaining))
	for x := 0; x < width && *remaining > 0; x++ {
		cell := at(x)
		if cell == nil || cell.Width <= 0 {
			continue
		}
		concealed := cell.Style.Attrs&uv.AttrConceal != 0
		content := cell.Content
		if concealed || content == "" {
			content = strings.Repeat(" ", cell.Width)
		}
		count := validUTF8PrefixBytes(content, *remaining)
		row = append(row, textCell{content: strings.Clone(content[:count])})
		*remaining -= count
	}
	return row
}

func (snapshot TextSnapshot) Render() []string {
	lines := make([]string, 0, len(snapshot.rows))
	for _, row := range snapshot.rows {
		var line strings.Builder
		for _, cell := range row {
			line.WriteString(cell.content)
		}
		text := strings.TrimRight(line.String(), " ")
		text = strings.Map(withoutTerminalControls, text)
		lines = append(lines, text)
	}
	return lines
}

func withoutTerminalControls(r rune) rune {
	if unicode.IsControl(r) {
		return -1
	}
	return r
}
