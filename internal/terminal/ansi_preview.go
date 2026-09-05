package terminal

import (
	"fmt"
	"io"
	"strings"
)

// PreviewANSIOutput replays raw terminal output on a blank viewport and returns
// the final rendered cells. It preserves only presentation carried by the ANSI
// stream; it does not infer styles from the resulting text.
//
// A caller replaying a log suffix must treat the result as best effort because
// the suffix may begin after terminal state that affected its first bytes.
func PreviewANSIOutput(output []byte, cols, rows int) (Preview, error) {
	return PreviewANSIOutputAtSize(output, cols, rows, cols, rows)
}

// PreviewANSIOutputAtSize replays output using the source terminal dimensions,
// then returns a separately bounded crop. Keeping those dimensions distinct is
// required for faithful wrapping before a smaller picker preview is requested.
func PreviewANSIOutputAtSize(output []byte, screenCols, screenRows, previewCols, previewRows int) (Preview, error) {
	if screenCols <= 0 || screenRows <= 0 || previewCols <= 0 || previewRows <= 0 {
		return Preview{}, fmt.Errorf(
			"terminal: ANSI screen and preview dimensions must be positive, got screen %dx%d preview %dx%d",
			screenCols,
			screenRows,
			previewCols,
			previewRows,
		)
	}

	screen := NewScreen(screenCols, screenRows)
	written, err := screen.Write(output)
	if err != nil {
		return Preview{}, fmt.Errorf("terminal: replay ANSI preview: %w", err)
	}
	if written != len(output) {
		return Preview{}, fmt.Errorf("terminal: replay ANSI preview: %w", io.ErrShortWrite)
	}
	return screen.Preview(previewCols, previewRows), nil
}

// MatchPreviewStyles projects presentation from a replay onto an authoritative
// plain-text inspection. It succeeds only when every row matches exactly after
// removing the replay emulator's trailing blank cells. This lets compatibility
// callers recover styles carried by raw terminal output without replacing or
// guessing any inspected text.
func MatchPreviewStyles(replayed Preview, plain []string) ([]PreviewLine, bool) {
	if len(replayed.Lines) != len(plain) || len(replayed.StyledLines) != len(plain) {
		return nil, false
	}

	matched := make([]PreviewLine, len(plain))
	hasPresentation := false
	for row, authoritative := range plain {
		if strings.TrimRight(replayed.Lines[row], " ") != authoritative {
			return nil, false
		}
		matched[row] = previewLinePrefix(replayed.StyledLines[row], len(authoritative))
		for _, run := range matched[row].Runs {
			if run.Style != (PreviewStyle{}) {
				hasPresentation = true
			}
		}
	}
	if !hasPresentation {
		return nil, false
	}
	return matched, true
}

func previewLinePrefix(line PreviewLine, bytes int) PreviewLine {
	trimmed := PreviewLine{Runs: make([]PreviewRun, 0, len(line.Runs))}
	remaining := bytes
	for _, run := range line.Runs {
		if remaining == 0 {
			break
		}
		text := run.Text
		if len(text) > remaining {
			text = text[:remaining]
		}
		if text != "" {
			trimmed.Runs = append(trimmed.Runs, PreviewRun{Text: text, Style: run.Style})
		}
		remaining -= len(text)
	}
	return trimmed
}
