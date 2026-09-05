package worker

import (
	"fmt"
	"net"
	"path"
	"time"
	"unicode/utf8"

	inspectionwire "github.com/shaul/mesh/internal/inspection"
	"github.com/shaul/mesh/internal/protocol"
)

type processObservation struct {
	directory string
	command   string
}

type processObserver func(ptyFD, leaderPID int) processObservation

// inspectSession observes the process before taking the worker lock. Platform
// process probes can involve filesystem or system-tool IO and must not delay
// PTY output, attachment ownership changes, or shutdown.
func (w *Worker) inspectSession(cols, rows int) protocol.SessionInspection {
	process := w.currentProcessObservation()
	if !path.IsAbs(process.directory) {
		process.directory = ""
	}

	w.mu.Lock()
	preview := w.screen.Preview(cols, rows)
	if len(preview.Lines) > rows {
		preview.Lines = preview.Lines[:rows]
		if len(preview.StyledLines) >= rows {
			preview.StyledLines = preview.StyledLines[:rows]
		}
	}
	attached := w.client != nil
	nested := w.nestedLocked()
	// JSON drops time.Time's monotonic reading. Compare the same wall-only
	// values that the next process will receive, then keep a backward host-clock
	// adjustment from making an otherwise valid live snapshot impossible.
	lastOutputAt := w.lastOutputAt.Round(0)
	observedAt := w.currentTime().Round(0)
	if !lastOutputAt.IsZero() && lastOutputAt.After(observedAt) {
		lastOutputAt = observedAt
	}
	w.mu.Unlock()

	inspection := protocol.SessionInspection{
		ObservedAt:        observedAt,
		ForegroundCommand: process.command,
		TerminalTitle:     preview.Title,
		Attached:          attached,
		Preview:           preview.Lines,
		StyledPreview:     inspectionwire.StyledPreview(preview),
		Nested:            nested,
		NestingSupported:  true,
	}
	if !lastOutputAt.IsZero() {
		inspection.LastOutputAt = &lastOutputAt
	}
	if process.directory != "" {
		inspection.CurrentDirectory = process.directory
		inspection.DirectorySource = protocol.DirectorySourceProcess
	} else if preview.Directory != "" {
		inspection.CurrentDirectory = preview.Directory
		inspection.DirectorySource = protocol.DirectorySourceTerminal
	}
	boundInspectionText(&inspection)
	return inspection
}

func (w *Worker) currentProcessObservation() processObservation {
	observer := w.observeProcess
	if observer == nil {
		observer = defaultProcessObserver
	}
	ptyFD := -1
	if w.pty != nil {
		ptyFD = int(w.pty.Fd())
	}

	w.mu.Lock()
	leaderPID := 0
	trackedProcess := w.cmd != nil && w.cmd.Process != nil
	if trackedProcess {
		if w.reaped {
			w.mu.Unlock()
			return processObservation{}
		}
		leaderPID = w.cmd.Process.Pid
	}
	w.mu.Unlock()

	observation := observer(ptyFD, leaderPID)
	if !trackedProcess {
		return observation
	}

	// A Unix PID cannot be reused until its former process is reaped. If Wait
	// crossed the unlocked system probe, its result may describe a new process
	// that inherited the same numeric PID and must not escape the worker.
	w.mu.Lock()
	stillOurs := !w.reaped && w.cmd != nil && w.cmd.Process != nil && w.cmd.Process.Pid == leaderPID
	w.mu.Unlock()
	if !stillOurs {
		return processObservation{}
	}
	return observation
}

func (w *Worker) currentTime() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// boundInspectionText keeps even Unicode-heavy grapheme content within the
// aggregate wire budget. The screen already bounds display cells; combining
// marks can still make a small number of cells occupy many bytes.
func boundInspectionText(inspection *protocol.SessionInspection) {
	remaining := protocol.MaxInspectionTextBytes
	inspection.CurrentDirectory = inspectionTextPrefix(inspection.CurrentDirectory, &remaining)
	if inspection.CurrentDirectory == "" {
		inspection.DirectorySource = ""
	}
	inspection.ForegroundCommand = inspectionTextPrefix(inspection.ForegroundCommand, &remaining)
	inspection.TerminalTitle = inspectionTextPrefix(inspection.TerminalTitle, &remaining)

	styled := inspection.StyledPreview
	keepStyles := len(styled) == len(inspection.Preview)
	lines := make([]string, 0, len(inspection.Preview))
	styledLines := make([]protocol.PreviewLine, 0, len(inspection.Preview))
	for row, line := range inspection.Preview {
		if remaining == 0 {
			break
		}
		prefix := inspectionTextPrefix(line, &remaining)
		lines = append(lines, prefix)
		if keepStyles {
			styledLines = append(styledLines, trimInspectionPreviewLine(styled[row], len(prefix)))
		}
		if len(prefix) < len(line) {
			break
		}
	}
	inspection.Preview = lines
	if keepStyles {
		inspection.StyledPreview = styledLines
	} else {
		inspection.StyledPreview = nil
	}
}

func trimInspectionPreviewLine(line protocol.PreviewLine, bytes int) protocol.PreviewLine {
	if bytes <= 0 {
		return protocol.PreviewLine{}
	}
	trimmed := protocol.PreviewLine{Runs: make([]protocol.PreviewRun, 0, len(line.Runs))}
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
			trimmed.Runs = append(trimmed.Runs, protocol.PreviewRun{Text: text, Style: run.Style})
		}
		remaining -= len(text)
	}
	return trimmed
}

func inspectionTextPrefix(value string, remaining *int) string {
	if value == "" || *remaining <= 0 {
		return ""
	}
	if utf8.ValidString(value) {
		if len(value) <= *remaining {
			*remaining -= len(value)
			return value
		}
		end := *remaining
		for end > 0 && !utf8.RuneStart(value[end]) {
			end--
		}
		*remaining -= end
		return value[:end]
	}

	result := make([]byte, 0, min(len(value), *remaining))
	for _, r := range value {
		size := utf8.RuneLen(r)
		if size > *remaining {
			break
		}
		result = utf8.AppendRune(result, r)
		*remaining -= size
	}
	return string(result)
}

func (w *Worker) writeInspection(conn net.Conn, request protocol.Control) {
	response := protocol.Control{
		Type:      protocol.TypeInspected,
		RequestID: request.RequestID,
		SessionID: w.cfg.ID,
	}
	if err := protocol.ValidateInspectDimensions(request.PreviewCols, request.PreviewRows); err != nil {
		response.Type = protocol.TypeError
		response.Message = fmt.Sprintf("inspect session: %v", err)
	} else {
		inspection := w.inspectSession(request.PreviewCols, request.PreviewRows)
		if err := protocol.ValidateSessionInspection(inspection); err != nil {
			response.Type = protocol.TypeError
			response.Message = fmt.Sprintf("inspect session: %v", err)
		} else {
			response.Inspection = &inspection
		}
	}

	_ = conn.SetWriteDeadline(time.Now().Add(attachmentWriteTimeout))
	defer conn.SetWriteDeadline(time.Time{}) //nolint:errcheck // one-shot connection closes next
	_ = protocol.NewWriter(conn).WriteControlMsg(response)
}
