package protocol

import (
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// Inspection limits bound one live session preview and its aggregate text.
const (
	MaxInspectionPreviewCols = 160
	MaxInspectionPreviewRows = 24
	MaxInspectionTextBytes   = 32 << 10
	MaxInspectionPreviewRuns = MaxInspectionPreviewCols * MaxInspectionPreviewRows
)

// PreviewColorKind identifies a default, ANSI, indexed, or RGB terminal color.
type PreviewColorKind uint8

const (
	PreviewColorDefault PreviewColorKind = iota
	PreviewColorBasic
	PreviewColorIndexed
	PreviewColorRGB
)

// PreviewColor retains terminal palette semantics. RGB values are packed as
// 0xRRGGBB; basic and indexed values retain their terminal palette index.
type PreviewColor struct {
	Kind  PreviewColorKind `json:"kind,omitempty"`
	Value uint32           `json:"value,omitempty"`
}

// PreviewUnderline is the underline shape emitted by the inspected terminal.
type PreviewUnderline uint8

const (
	PreviewUnderlineNone PreviewUnderline = iota
	PreviewUnderlineSingle
	PreviewUnderlineDouble
	PreviewUnderlineCurly
	PreviewUnderlineDotted
	PreviewUnderlineDashed
)

// PreviewStyle is a safe subset of terminal cell presentation. It cannot carry
// cursor movement, links, blinking, concealment, or arbitrary escape codes.
type PreviewStyle struct {
	Foreground     PreviewColor     `json:"foreground,omitempty"`
	Background     PreviewColor     `json:"background,omitempty"`
	UnderlineColor PreviewColor     `json:"underlineColor,omitempty"`
	Bold           bool             `json:"bold,omitempty"`
	Faint          bool             `json:"faint,omitempty"`
	Italic         bool             `json:"italic,omitempty"`
	Reverse        bool             `json:"reverse,omitempty"`
	Strikethrough  bool             `json:"strikethrough,omitempty"`
	Underline      PreviewUnderline `json:"underline,omitempty"`
}

// PreviewRun is control-free text rendered with one presentation.
type PreviewRun struct {
	Text  string       `json:"text"`
	Style PreviewStyle `json:"style,omitempty"`
}

// PreviewLine coalesces adjacent cells that share one presentation.
type PreviewLine struct {
	Runs []PreviewRun `json:"runs,omitempty"`
}

// DirectorySource identifies how a live current directory was observed.
type DirectorySource string

const (
	DirectorySourceProcess  DirectorySource = "process"
	DirectorySourceTerminal DirectorySource = "terminal"
)

// SessionInspection is one bounded, read-only observation of a live session.
type SessionInspection struct {
	ObservedAt        time.Time         `json:"observedAt"`
	CurrentDirectory  string            `json:"currentDirectory,omitempty"`
	DirectorySource   DirectorySource   `json:"directorySource,omitempty"`
	ForegroundCommand string            `json:"foregroundCommand,omitempty"`
	TerminalTitle     string            `json:"terminalTitle,omitempty"`
	LastOutputAt      *time.Time        `json:"lastOutputAt,omitempty"`
	Attached          bool              `json:"attached"`
	Preview           []string          `json:"preview,omitempty"`
	StyledPreview     []PreviewLine     `json:"styledPreview,omitempty"`
	Nested            []SessionIdentity `json:"nested,omitempty"`
	NestingSupported  bool              `json:"nestingSupported,omitempty"`
}

// ValidateInspectDimensions checks the requested bounds for a session preview.
func ValidateInspectDimensions(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("protocol: inspection dimensions must be positive, got %dx%d", cols, rows)
	}
	if cols > MaxInspectionPreviewCols || rows > MaxInspectionPreviewRows {
		return fmt.Errorf(
			"protocol: inspection dimensions %dx%d exceed the limit of %dx%d",
			cols,
			rows,
			MaxInspectionPreviewCols,
			MaxInspectionPreviewRows,
		)
	}
	return nil
}

// ValidateSessionInspection checks an inspection received at a protocol
// boundary. It does not compare the enclosing control message's session ID.
func ValidateSessionInspection(inspection SessionInspection) error {
	if err := ValidateNestedSessions(inspection.Nested); err != nil {
		return fmt.Errorf("protocol: inspection nesting: %w", err)
	}
	if inspection.ObservedAt.IsZero() {
		return fmt.Errorf("protocol: inspection has an invalid observation time")
	}
	if inspection.LastOutputAt != nil {
		if inspection.LastOutputAt.IsZero() {
			return fmt.Errorf("protocol: inspection has an invalid last-output time")
		}
		if inspection.LastOutputAt.After(inspection.ObservedAt) {
			return fmt.Errorf("protocol: inspection last-output time is after its observation time")
		}
	}

	if inspection.CurrentDirectory == "" && inspection.DirectorySource != "" {
		return fmt.Errorf("protocol: inspection directory source requires a current directory")
	}
	if inspection.CurrentDirectory != "" && inspection.DirectorySource == "" {
		return fmt.Errorf("protocol: inspection current directory requires a source")
	}
	if inspection.CurrentDirectory != "" && !path.IsAbs(inspection.CurrentDirectory) {
		return fmt.Errorf("protocol: inspection current directory must be absolute")
	}
	switch inspection.DirectorySource {
	case "", DirectorySourceProcess, DirectorySourceTerminal:
	default:
		return fmt.Errorf("protocol: inspection has unknown directory source %q", inspection.DirectorySource)
	}

	if len(inspection.Preview) > MaxInspectionPreviewRows {
		return fmt.Errorf(
			"protocol: inspection preview has %d rows, limit is %d",
			len(inspection.Preview),
			MaxInspectionPreviewRows,
		)
	}
	textBytes := len(inspection.CurrentDirectory) + len(inspection.ForegroundCommand) + len(inspection.TerminalTitle)
	for row, line := range inspection.Preview {
		textBytes += len(line)
		for _, r := range line {
			if unicode.IsControl(r) {
				return fmt.Errorf("protocol: inspection preview row %d contains terminal control characters", row)
			}
		}
		if columns := ansi.StringWidth(line); columns > MaxInspectionPreviewCols {
			return fmt.Errorf(
				"protocol: inspection preview row %d has %d columns, limit is %d",
				row,
				columns,
				MaxInspectionPreviewCols,
			)
		}
	}
	if err := validateStyledPreview(inspection); err != nil {
		return err
	}
	if textBytes > MaxInspectionTextBytes {
		return fmt.Errorf(
			"protocol: inspection text has %d bytes, limit is %d",
			textBytes,
			MaxInspectionTextBytes,
		)
	}
	return nil
}

func validateStyledPreview(inspection SessionInspection) error {
	if len(inspection.StyledPreview) == 0 {
		return nil
	}
	if len(inspection.StyledPreview) != len(inspection.Preview) {
		return fmt.Errorf(
			"protocol: styled inspection preview has %d rows, plain preview has %d",
			len(inspection.StyledPreview),
			len(inspection.Preview),
		)
	}

	runCount := 0
	for row, line := range inspection.StyledPreview {
		var plain strings.Builder
		for column, run := range line.Runs {
			runCount++
			if runCount > MaxInspectionPreviewRuns {
				return fmt.Errorf("protocol: styled inspection preview exceeds %d runs", MaxInspectionPreviewRuns)
			}
			if run.Text == "" {
				return fmt.Errorf("protocol: styled inspection preview row %d run %d is empty", row, column)
			}
			if err := validatePreviewStyle(run.Style); err != nil {
				return fmt.Errorf("protocol: styled inspection preview row %d run %d: %w", row, column, err)
			}
			for _, character := range run.Text {
				if unicode.IsControl(character) {
					return fmt.Errorf("protocol: styled inspection preview row %d run %d contains terminal control characters", row, column)
				}
			}
			plain.WriteString(run.Text)
		}
		if plain.String() != inspection.Preview[row] {
			return fmt.Errorf("protocol: styled inspection preview row %d does not match its plain text", row)
		}
	}
	return nil
}

func validatePreviewStyle(style PreviewStyle) error {
	for name, color := range map[string]PreviewColor{
		"foreground":      style.Foreground,
		"background":      style.Background,
		"underline color": style.UnderlineColor,
	} {
		switch color.Kind {
		case PreviewColorDefault:
			if color.Value != 0 {
				return fmt.Errorf("default %s has value %d", name, color.Value)
			}
		case PreviewColorBasic:
			if color.Value > 15 {
				return fmt.Errorf("basic %s index %d exceeds 15", name, color.Value)
			}
		case PreviewColorIndexed:
			if color.Value > 255 {
				return fmt.Errorf("indexed %s value %d exceeds 255", name, color.Value)
			}
		case PreviewColorRGB:
			if color.Value > 0xffffff {
				return fmt.Errorf("RGB %s value %#x exceeds 0xffffff", name, color.Value)
			}
		default:
			return fmt.Errorf("%s has unknown color kind %d", name, color.Kind)
		}
	}
	if style.Underline > PreviewUnderlineDashed {
		return fmt.Errorf("unknown underline style %d", style.Underline)
	}
	return nil
}
