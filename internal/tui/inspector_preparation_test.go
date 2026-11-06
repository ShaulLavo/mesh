package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/cli"
	"github.com/shaul/mesh/internal/protocol"
)

func preparedPreviewFixture(t *testing.T) model {
	t.Helper()
	observed := pickerTestNow.Add(-5 * time.Hour)
	lastOutput := observed.Add(-12 * time.Second)
	value := cli.SessionInspection{
		ObservedAt: observed, LastOutputAt: &lastOutput,
		CurrentDirectory: "/work/current", DirectorySource: cli.SessionDirectoryProcess,
		ForegroundCommand: "go test", TerminalTitle: "original title",
	}
	for row := range inspectionPreviewRows {
		text := fmt.Sprintf("row %02d 界 café e\u0301 🤸‍♂️", row+1)
		value.Preview = append(value.Preview, text)
		value.StyledPreview = append(value.StyledPreview, protocol.PreviewLine{Runs: []protocol.PreviewRun{{
			Text: text, Style: protocol.PreviewStyle{Bold: true, Foreground: protocol.PreviewColor{Kind: protocol.PreviewColorBasic, Value: 2}},
		}}})
	}
	m := newPickerModel(context.Background(), cli.PickerInput{
		Hosts: []cli.HostSessions{{
			Host:     cli.HostRecord{ID: "preview-host", MachineName: "pc"},
			Sessions: []protocol.SessionInfo{{ID: "preview-session", State: "detached", Cwd: "/work/launch", Command: []string{"launch-command"}, CreatedAt: pickerTestNow}},
		}},
		Inspect: func(context.Context, cli.PickerInspectRequest) (cli.SessionInspection, error) { return value, nil },
	}, pickerTestNow)
	m.width, m.height = 160, 45
	m.enterSessions(0)
	command := m.inspectSelected()
	if command == nil {
		t.Fatal("fixture has no inspection command")
	}
	m = m.applyInspection(command().(inspectionResultMsg))
	t.Cleanup(func() { m.invalidateSessionScreenLoops() })
	return m
}

func TestInspectionPreviewPreparesOnlyVisibleTail(t *testing.T) {
	m := preparedPreviewFixture(t)
	value := m.inspection.value
	for _, rows := range []int{0, 1, 4, inspectionPreviewRows, inspectionPreviewRows + 5} {
		lines, styled := m.renderInspectionPreview(value, rows)
		want := tailRows(value.Preview, rows)
		if !styled || len(lines) != len(want) {
			t.Fatalf("rows %d: got %d lines, styled %t", rows, len(lines), styled)
		}
		assertStyledPreviewTail(t, lines, want)
	}
}

func assertStyledPreviewTail(t *testing.T, lines, want []string) {
	t.Helper()
	for row, line := range lines {
		if ansi.Strip(line) != want[row] || line == ansi.Strip(line) {
			t.Fatalf("row %d lost text or style: %q, want %q", row, line, want[row])
		}
	}
}

func TestInspectionPreviewValidatesOffscreenRowsBeforeClipping(t *testing.T) {
	for _, mismatch := range []string{"first", "last", "line count"} {
		t.Run(mismatch, func(t *testing.T) { assertPreviewMismatch(t, mismatch) })
	}
}

func assertPreviewMismatch(t *testing.T, mismatch string) {
	t.Helper()
	m := preparedPreviewFixture(t)
	value := m.inspection.value
	last := len(value.Preview) - 1
	value.Preview[last] += "\x1b[31m\u202e"
	value.StyledPreview[last].Runs[0].Text = value.Preview[last]
	if mismatch == "line count" {
		value.StyledPreview = value.StyledPreview[1:]
	} else {
		row := 0
		if mismatch == "last" {
			row = len(value.Preview) - 1
		}
		value.StyledPreview[row].Runs[0].Text = "mismatch"
	}
	lines, styled := m.renderInspectionPreview(value, 2)
	if styled || len(lines) != 2 {
		t.Fatalf("mismatch %s did not fall back: %#v, styled %t", mismatch, lines, styled)
	}
	for row, line := range lines {
		want := safeText(value.Preview[len(value.Preview)-2+row])
		if line != want {
			t.Fatalf("mismatch %s row %d: got %q, want safe plain %q", mismatch, row, line, want)
		}
	}
}

func TestInspectionPreparationKeepsCurrentFactsAcrossViews(t *testing.T) {
	m := preparedPreviewFixture(t)
	before := m.View().Content
	for _, text := range []string{"original title", "current path", "/work/current", "process", "go test", "launch-command", "output 12s ago"} {
		assertInspectionFrameContains(t, before, text)
	}
	m.now = m.now.Add(10 * time.Minute)
	m.inspection.value.TerminalTitle = "changed title"
	m.inspection.value.CurrentDirectory = "/work/changed"
	m.inspection.value.DirectorySource = cli.SessionDirectoryTerminal
	m.inspection.value.ForegroundCommand = "changed command"
	m.inspection.value.Preview[len(m.inspection.value.Preview)-1] = "changed terminal output"
	m.inspection.value.StyledPreview = nil
	for _, text := range []string{"changed title", "/work/changed", "terminal", "changed command", "changed terminal output", "output 10m ago"} {
		assertInspectionFrameContains(t, m.View().Content, text)
	}
	m.fullPreview = true
	full := m.View().Content
	for _, text := range []string{"changed title", "current path /work/changed", "changed terminal output"} {
		assertInspectionFrameContains(t, full, text)
	}
	if strings.Contains(full, "original title") || strings.Contains(full, "/work/current") {
		t.Fatalf("full preview retained old facts:\n%s", ansi.Strip(full))
	}
}

func assertInspectionFrameContains(t *testing.T, frame, text string) {
	t.Helper()
	if !strings.Contains(ansi.Strip(frame), text) {
		t.Fatalf("frame lacks %q:\n%s", text, ansi.Strip(frame))
	}
}

func TestInspectionPreparationFollowsInlineAndFullResize(t *testing.T) {
	m := preparedPreviewFixture(t)
	for _, full := range []bool{false, true, false} {
		m.fullPreview = full
		for _, size := range [][2]int{{80, 24}, {160, 45}, {40, 12}, {160, 45}} {
			assertPreparedResize(t, &m, size)
		}
	}
}

func assertPreparedResize(t *testing.T, m *model, size [2]int) {
	t.Helper()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
	*m = updated.(model)
	frame := m.View().Content
	assertFits(t, frame, size[0], size[1])
	rows := max(0, size[1]-6-len(m.updateNoticeLines()))
	show := size[1] >= 6
	if !m.fullPreview {
		_, rows, show = m.sessionLayout(max(0, size[1]-4-len(m.updateNoticeLines())))
	}
	if !show || rows <= 0 {
		if strings.Contains(ansi.Strip(frame), "row 24") {
			t.Fatal("preview appeared without room")
		}
		return
	}
	assertInspectionFrameContains(t, frame, "row 24")
	first := max(1, inspectionPreviewRows-rows+1)
	assertInspectionFrameContains(t, frame, fmt.Sprintf("row %02d", first))
	if first > 1 && strings.Contains(ansi.Strip(frame), fmt.Sprintf("row %02d", first-1)) {
		t.Fatalf("offscreen row survived resize to %dx%d", size[0], size[1])
	}
}

func TestInspectionPreparationChecksBlanknessAcrossWholeSource(t *testing.T) {
	m := preparedPreviewFixture(t)
	m.inspection.value.StyledPreview = nil
	for row := range m.inspection.value.Preview {
		m.inspection.value.Preview[row] = " "
	}
	m.inspection.value.Preview[0] = "offscreen content"
	_, current, _ := m.currentSession()
	details := m.detailsFor(current, 2)
	if len(details.preview) != 2 || details.preview[0] != " " || details.preview[1] != " " {
		t.Fatalf("nonblank source was replaced after clipping: %#v", details.preview)
	}
	m.inspection.value.Preview[0] = " "
	details = m.detailsFor(current, 2)
	if len(details.preview) != 1 || details.preview[0] != "(screen is blank)" || details.previewStyled {
		t.Fatalf("blank source lost its placeholder: %#v", details)
	}
}
