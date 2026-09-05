package protocol

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestInspectionControlRoundTrip(t *testing.T) {
	observedAt := time.Date(2026, time.September, 4, 9, 0, 0, 0, time.UTC)
	lastOutputAt := observedAt.Add(-3 * time.Second)
	want := Control{
		Type:        TypeInspected,
		RequestID:   "inspect-1",
		SessionID:   "7K3D",
		PreviewCols: 120,
		PreviewRows: 20,
		Inspection: &SessionInspection{
			ObservedAt:        observedAt,
			CurrentDirectory:  "/home/me/mesh",
			DirectorySource:   DirectorySourceTerminal,
			ForegroundCommand: "go test ./internal/protocol",
			TerminalTitle:     "mesh tests",
			LastOutputAt:      &lastOutputAt,
			Attached:          true,
			Preview:           []string{"$ go test ./internal/protocol", "ok  github.com/shaul/mesh/internal/protocol"},
			StyledPreview: []PreviewLine{
				{Runs: []PreviewRun{{
					Text: "$ go test ./internal/protocol",
					Style: PreviewStyle{
						Foreground: PreviewColor{Kind: PreviewColorBasic, Value: 2},
						Bold:       true,
					},
				}}},
				{Runs: []PreviewRun{{Text: "ok  github.com/shaul/mesh/internal/protocol"}}},
			},
		},
	}

	payload, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeControl(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded control = %#v, want %#v", got, want)
	}
}

func TestInspectRequestControlRoundTrip(t *testing.T) {
	want := Control{
		Type:        TypeInspect,
		RequestID:   "inspect-2",
		SessionID:   "91AZ",
		PreviewCols: MaxInspectionPreviewCols,
		PreviewRows: MaxInspectionPreviewRows,
	}

	payload, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeControl(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded control = %#v, want %#v", got, want)
	}
}

func TestValidateInspectDimensions(t *testing.T) {
	for _, test := range []struct {
		name      string
		cols      int
		rows      int
		wantError bool
	}{
		{name: "smallest", cols: 1, rows: 1},
		{name: "largest", cols: MaxInspectionPreviewCols, rows: MaxInspectionPreviewRows},
		{name: "missing columns", cols: 0, rows: 1, wantError: true},
		{name: "negative columns", cols: -1, rows: 1, wantError: true},
		{name: "too many columns", cols: MaxInspectionPreviewCols + 1, rows: 1, wantError: true},
		{name: "missing rows", cols: 1, rows: 0, wantError: true},
		{name: "negative rows", cols: 1, rows: -1, wantError: true},
		{name: "too many rows", cols: 1, rows: MaxInspectionPreviewRows + 1, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateInspectDimensions(test.cols, test.rows)
			if (err != nil) != test.wantError {
				t.Fatalf("ValidateInspectDimensions(%d, %d) error = %v, want error %v", test.cols, test.rows, err, test.wantError)
			}
		})
	}
}

func TestValidateSessionInspection(t *testing.T) {
	observedAt := time.Date(2026, time.September, 4, 9, 0, 0, 0, time.UTC)
	lastOutputAt := observedAt.Add(-time.Second)
	valid := SessionInspection{
		ObservedAt:        observedAt,
		CurrentDirectory:  "/home/me/mesh",
		DirectorySource:   DirectorySourceProcess,
		ForegroundCommand: "go test ./...",
		TerminalTitle:     "mesh",
		LastOutputAt:      &lastOutputAt,
		Attached:          true,
		Preview:           []string{"$ go test ./...", "ok"},
	}
	if err := ValidateSessionInspection(valid); err != nil {
		t.Fatalf("ValidateSessionInspection(valid) error = %v", err)
	}

	withoutOptionalFields := SessionInspection{ObservedAt: observedAt}
	if err := ValidateSessionInspection(withoutOptionalFields); err != nil {
		t.Fatalf("ValidateSessionInspection(without optional fields) error = %v", err)
	}

	lastAtObservation := observedAt
	withBoundaryTime := SessionInspection{ObservedAt: observedAt, LastOutputAt: &lastAtObservation}
	if err := ValidateSessionInspection(withBoundaryTime); err != nil {
		t.Fatalf("ValidateSessionInspection(last output at observation) error = %v", err)
	}

	atTextLimit := SessionInspection{ObservedAt: observedAt, TerminalTitle: strings.Repeat("x", MaxInspectionTextBytes)}
	if err := ValidateSessionInspection(atTextLimit); err != nil {
		t.Fatalf("ValidateSessionInspection(at text limit) error = %v", err)
	}

	atPreviewWidth := SessionInspection{ObservedAt: observedAt, Preview: []string{strings.Repeat("界", MaxInspectionPreviewCols/2)}}
	if err := ValidateSessionInspection(atPreviewWidth); err != nil {
		t.Fatalf("ValidateSessionInspection(at preview width) error = %v", err)
	}

	styled := SessionInspection{
		ObservedAt: observedAt,
		Preview:    []string{"red indexed rgb"},
		StyledPreview: []PreviewLine{{Runs: []PreviewRun{
			{Text: "red ", Style: PreviewStyle{Foreground: PreviewColor{Kind: PreviewColorBasic, Value: 1}, Bold: true}},
			{Text: "indexed ", Style: PreviewStyle{Background: PreviewColor{Kind: PreviewColorIndexed, Value: 236}, Italic: true}},
			{Text: "rgb", Style: PreviewStyle{UnderlineColor: PreviewColor{Kind: PreviewColorRGB, Value: 0x123456}, Underline: PreviewUnderlineCurly}},
		}}},
	}
	if err := ValidateSessionInspection(styled); err != nil {
		t.Fatalf("ValidateSessionInspection(styled) error = %v", err)
	}
}

func TestValidateSessionInspectionRejectsInvalidBoundaries(t *testing.T) {
	observedAt := time.Date(2026, time.September, 4, 9, 0, 0, 0, time.UTC)
	afterObservation := observedAt.Add(time.Nanosecond)
	zeroTime := time.Time{}

	for _, test := range []struct {
		name       string
		inspection SessionInspection
	}{
		{name: "missing observation time", inspection: SessionInspection{}},
		{name: "zero last output time", inspection: SessionInspection{ObservedAt: observedAt, LastOutputAt: &zeroTime}},
		{name: "last output after observation", inspection: SessionInspection{ObservedAt: observedAt, LastOutputAt: &afterObservation}},
		{name: "directory without source", inspection: SessionInspection{ObservedAt: observedAt, CurrentDirectory: "/tmp"}},
		{name: "relative directory", inspection: SessionInspection{ObservedAt: observedAt, CurrentDirectory: "tmp", DirectorySource: DirectorySourceProcess}},
		{name: "source without directory", inspection: SessionInspection{ObservedAt: observedAt, DirectorySource: DirectorySourceProcess}},
		{name: "unknown directory source", inspection: SessionInspection{ObservedAt: observedAt, CurrentDirectory: "/tmp", DirectorySource: DirectorySource("shell")}},
		{name: "too many preview rows", inspection: SessionInspection{ObservedAt: observedAt, Preview: make([]string, MaxInspectionPreviewRows+1)}},
		{name: "preview row too wide", inspection: SessionInspection{ObservedAt: observedAt, Preview: []string{strings.Repeat("x", MaxInspectionPreviewCols+1)}}},
		{name: "wide preview row too wide", inspection: SessionInspection{ObservedAt: observedAt, Preview: []string{strings.Repeat("界", MaxInspectionPreviewCols/2+1)}}},
		{name: "text over limit", inspection: SessionInspection{ObservedAt: observedAt, ForegroundCommand: strings.Repeat("x", MaxInspectionTextBytes+1)}},
		{name: "escape in preview", inspection: SessionInspection{ObservedAt: observedAt, Preview: []string{"safe\x1b[31munsafe"}}},
		{name: "newline in preview", inspection: SessionInspection{ObservedAt: observedAt, Preview: []string{"first\nsecond"}}},
		{name: "c1 control in preview", inspection: SessionInspection{ObservedAt: observedAt, Preview: []string{"safe\u009bunsafe"}}},
		{name: "styled row count differs", inspection: SessionInspection{ObservedAt: observedAt, Preview: []string{"one"}, StyledPreview: []PreviewLine{{}, {}}}},
		{name: "styled text differs", inspection: SessionInspection{ObservedAt: observedAt, Preview: []string{"one"}, StyledPreview: []PreviewLine{{Runs: []PreviewRun{{Text: "two"}}}}}},
		{name: "empty styled run", inspection: SessionInspection{ObservedAt: observedAt, Preview: []string{""}, StyledPreview: []PreviewLine{{Runs: []PreviewRun{{}}}}}},
		{name: "control in styled run", inspection: SessionInspection{ObservedAt: observedAt, Preview: []string{"safe"}, StyledPreview: []PreviewLine{{Runs: []PreviewRun{{Text: "safe\x1b"}}}}}},
		{name: "invalid basic color", inspection: styledInspection(observedAt, PreviewStyle{Foreground: PreviewColor{Kind: PreviewColorBasic, Value: 16}})},
		{name: "invalid indexed color", inspection: styledInspection(observedAt, PreviewStyle{Background: PreviewColor{Kind: PreviewColorIndexed, Value: 256}})},
		{name: "invalid rgb color", inspection: styledInspection(observedAt, PreviewStyle{UnderlineColor: PreviewColor{Kind: PreviewColorRGB, Value: 0x1000000}})},
		{name: "unknown color kind", inspection: styledInspection(observedAt, PreviewStyle{Foreground: PreviewColor{Kind: PreviewColorKind(99)}})},
		{name: "unknown underline", inspection: styledInspection(observedAt, PreviewStyle{Underline: PreviewUnderline(99)})},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateSessionInspection(test.inspection); err == nil {
				t.Fatal("ValidateSessionInspection() error = nil, want error")
			}
		})
	}
}

func styledInspection(observedAt time.Time, style PreviewStyle) SessionInspection {
	return SessionInspection{
		ObservedAt:    observedAt,
		Preview:       []string{"x"},
		StyledPreview: []PreviewLine{{Runs: []PreviewRun{{Text: "x", Style: style}}}},
	}
}
