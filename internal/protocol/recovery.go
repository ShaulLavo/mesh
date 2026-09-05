package protocol

import (
	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/recovery"
)

const (
	TypeRecover                = "session.recover"
	TypeRecovered              = "session.recovered"
	TypeRecoveryRead           = "session.recovery"
	TypeRecoveryRecord         = "session.recovery-record"
	TypeRecoveryCommand        = "session.recovery-command"
	TypeShellUpdate            = "session.shell-update"
	TypeShellUpdated           = "session.shell-updated"
	ErrorCodeRecoveryUncertain = "recovery.uncertain"
)

// RecoveryPreview bounds saved text to the same dimensions as live inspection.
func RecoveryPreview(record recovery.Record) recovery.Record {
	lines := record.Lines
	if len(lines) > MaxInspectionPreviewRows {
		lines = lines[len(lines)-MaxInspectionPreviewRows:]
	}
	record.Lines = make([]string, 0, len(lines))
	remaining := MaxInspectionTextBytes
	for _, line := range lines {
		line = ansi.Truncate(line, MaxInspectionPreviewCols, "")
		if len(line) > remaining {
			break
		}
		record.Lines = append(record.Lines, line)
		remaining -= len(line)
	}
	return record
}
