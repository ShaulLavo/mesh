// Package inspection maps terminal observations onto the bounded wire model.
package inspection

import (
	"github.com/shaul/mesh/internal/protocol"
	terminalstate "github.com/shaul/mesh/internal/terminal"
)

// StyledPreview converts safe terminal-cell presentation to protocol runs.
func StyledPreview(preview terminalstate.Preview) []protocol.PreviewLine {
	if len(preview.StyledLines) != len(preview.Lines) {
		return nil
	}
	lines := make([]protocol.PreviewLine, len(preview.StyledLines))
	for row, line := range preview.StyledLines {
		lines[row].Runs = make([]protocol.PreviewRun, len(line.Runs))
		for column, run := range line.Runs {
			lines[row].Runs[column] = protocol.PreviewRun{
				Text:  run.Text,
				Style: previewStyle(run.Style),
			}
		}
	}
	return lines
}

func previewStyle(style terminalstate.PreviewStyle) protocol.PreviewStyle {
	return protocol.PreviewStyle{
		Foreground:     previewColor(style.Foreground),
		Background:     previewColor(style.Background),
		UnderlineColor: previewColor(style.UnderlineColor),
		Bold:           style.Bold,
		Faint:          style.Faint,
		Italic:         style.Italic,
		Reverse:        style.Reverse,
		Strikethrough:  style.Strikethrough,
		Underline:      previewUnderline(style.Underline),
	}
}

func previewColor(color terminalstate.PreviewColor) protocol.PreviewColor {
	var kind protocol.PreviewColorKind
	switch color.Kind {
	case terminalstate.PreviewColorDefault:
		kind = protocol.PreviewColorDefault
	case terminalstate.PreviewColorBasic:
		kind = protocol.PreviewColorBasic
	case terminalstate.PreviewColorIndexed:
		kind = protocol.PreviewColorIndexed
	case terminalstate.PreviewColorRGB:
		kind = protocol.PreviewColorRGB
	default:
		// Validation rejects this sentinel if the terminal model grows without
		// an explicit wire-format mapping.
		kind = ^protocol.PreviewColorKind(0)
	}
	return protocol.PreviewColor{Kind: kind, Value: color.Value}
}

func previewUnderline(underline terminalstate.PreviewUnderline) protocol.PreviewUnderline {
	switch underline {
	case terminalstate.PreviewUnderlineNone:
		return protocol.PreviewUnderlineNone
	case terminalstate.PreviewUnderlineSingle:
		return protocol.PreviewUnderlineSingle
	case terminalstate.PreviewUnderlineDouble:
		return protocol.PreviewUnderlineDouble
	case terminalstate.PreviewUnderlineCurly:
		return protocol.PreviewUnderlineCurly
	case terminalstate.PreviewUnderlineDotted:
		return protocol.PreviewUnderlineDotted
	case terminalstate.PreviewUnderlineDashed:
		return protocol.PreviewUnderlineDashed
	default:
		return ^protocol.PreviewUnderline(0)
	}
}
