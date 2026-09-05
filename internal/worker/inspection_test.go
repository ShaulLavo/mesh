package worker

import (
	"net"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	terminalstate "github.com/shaul/mesh/internal/terminal"
)

func TestInspectReturnsReadOnlySnapshotWithProcessPrecedence(t *testing.T) {
	sid, err := protocol.NewSessionID("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	observedAt := time.Date(2026, time.September, 4, 9, 0, 0, 0, time.UTC)
	lastOutputAt := observedAt.Add(-18 * time.Second)
	active := &attachment{}
	screen := &recordingScreen{
		cols: 91,
		rows: 33,
		preview: terminalstate.Preview{
			Lines:     []string{"$ go test ./...", "ok  github.com/shaul/mesh"},
			Title:     "mesh tests",
			Directory: "/terminal/mesh",
		},
	}
	observedFD := -1
	observedLeader := -1
	w := &Worker{
		cfg:          Config{ID: sid.String()},
		sid:          sid,
		pty:          newPipePTY(),
		cmd:          &exec.Cmd{Process: &os.Process{Pid: 4321}},
		ring:         session.NewRing(ringSize),
		screen:       screen,
		client:       active,
		lastOutputAt: lastOutputAt,
		now:          func() time.Time { return observedAt },
		observeProcess: func(ptyFD, leaderPID int) processObservation {
			observedFD = ptyFD
			observedLeader = leaderPID
			return processObservation{directory: "/process/mesh", command: "go"}
		},
	}
	defer w.pty.Close() //nolint:errcheck // test resource cleanup

	response := inspectRequest(t, w, protocol.Control{
		Type:        protocol.TypeInspect,
		RequestID:   "inspect-1",
		SessionID:   sid.String(),
		PreviewCols: 80,
		PreviewRows: 6,
	})

	if response.Type != protocol.TypeInspected || response.RequestID != "inspect-1" || response.SessionID != sid.String() {
		t.Fatalf("inspect response envelope = %+v", response)
	}
	if response.Inspection == nil {
		t.Fatal("inspect response has no inspection")
	}
	inspection := *response.Inspection
	if !inspection.ObservedAt.Equal(observedAt) || inspection.CurrentDirectory != "/process/mesh" || inspection.DirectorySource != protocol.DirectorySourceProcess {
		t.Fatalf("inspection observation = %+v", inspection)
	}
	if inspection.ForegroundCommand != "go" || inspection.TerminalTitle != "mesh tests" || !inspection.Attached {
		t.Fatalf("inspection process/title/attachment = %+v", inspection)
	}
	if inspection.LastOutputAt == nil || !inspection.LastOutputAt.Equal(lastOutputAt) {
		t.Fatalf("inspection last output = %v, want %v", inspection.LastOutputAt, lastOutputAt)
	}
	if strings.Join(inspection.Preview, "\n") != "$ go test ./...\nok  github.com/shaul/mesh" {
		t.Fatalf("inspection preview = %q", inspection.Preview)
	}
	if observedFD != 0 || observedLeader != 4321 {
		t.Fatalf("process observer target = fd %d leader %d", observedFD, observedLeader)
	}
	if screen.previewCols != 80 || screen.previewRows != 6 || screen.cols != 91 || screen.rows != 33 {
		t.Fatalf("inspection resized screen: preview %dx%d, terminal %dx%d", screen.previewCols, screen.previewRows, screen.cols, screen.rows)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client != active {
		t.Fatal("inspection replaced the active attacher")
	}
}

func TestInspectPreservesTerminalCellStylesAsStructuredRuns(t *testing.T) {
	screen := terminalstate.NewScreen(40, 4)
	if _, err := screen.Write([]byte("plain \x1b[1;38;5;45;48;2;1;2;3mstyled\x1b[0m")); err != nil {
		t.Fatal(err)
	}
	w := &Worker{
		screen: screen,
		now:    func() time.Time { return time.Date(2026, time.September, 5, 9, 0, 0, 0, time.UTC) },
		observeProcess: func(int, int) processObservation {
			return processObservation{}
		},
	}

	inspection := w.inspectSession(40, 4)
	if err := protocol.ValidateSessionInspection(inspection); err != nil {
		t.Fatal(err)
	}
	if len(inspection.StyledPreview) != 1 || len(inspection.StyledPreview[0].Runs) != 2 {
		t.Fatalf("styled preview = %#v", inspection.StyledPreview)
	}
	run := inspection.StyledPreview[0].Runs[1]
	if run.Text != "styled" || !run.Style.Bold ||
		run.Style.Foreground != (protocol.PreviewColor{Kind: protocol.PreviewColorIndexed, Value: 45}) ||
		run.Style.Background != (protocol.PreviewColor{Kind: protocol.PreviewColorRGB, Value: 0x010203}) {
		t.Fatalf("styled run = %#v", run)
	}
}

func TestInspectionSlicesStyledRowsWithPlainRows(t *testing.T) {
	observedAt := time.Date(2026, time.September, 5, 9, 1, 0, 0, time.UTC)
	w := &Worker{
		screen: &recordingScreen{preview: terminalstate.Preview{
			Lines: []string{"one", "two"},
			StyledLines: []terminalstate.PreviewLine{
				{Runs: []terminalstate.PreviewRun{{Text: "one"}}},
				{Runs: []terminalstate.PreviewRun{{Text: "two"}}},
			},
		}},
		now: func() time.Time { return observedAt },
		observeProcess: func(int, int) processObservation {
			return processObservation{}
		},
	}

	inspection := w.inspectSession(10, 1)
	if got, want := inspection.Preview, []string{"one"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("plain preview = %#v, want %#v", got, want)
	}
	if got, want := inspection.StyledPreview, []protocol.PreviewLine{{Runs: []protocol.PreviewRun{{Text: "one"}}}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("styled preview = %#v, want %#v", got, want)
	}
}

func TestInspectionDirectorySourcePrecedence(t *testing.T) {
	observedAt := time.Date(2026, time.September, 4, 9, 1, 0, 0, time.UTC)
	tests := []struct {
		name              string
		processDirectory  string
		terminalDirectory string
		wantDirectory     string
		wantSource        protocol.DirectorySource
	}{
		{
			name:              "process wins over terminal",
			processDirectory:  "/proc/current",
			terminalDirectory: "/osc/current",
			wantDirectory:     "/proc/current",
			wantSource:        protocol.DirectorySourceProcess,
		},
		{
			name:              "terminal is fallback",
			terminalDirectory: "/osc/current",
			wantDirectory:     "/osc/current",
			wantSource:        protocol.DirectorySourceTerminal,
		},
		{
			name:              "relative process directory is rejected",
			processDirectory:  "relative/process/path",
			terminalDirectory: "/osc/current",
			wantDirectory:     "/osc/current",
			wantSource:        protocol.DirectorySourceTerminal,
		},
		{name: "unknown remains unknown"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			order := make([]string, 0, 2)
			screen := &recordingScreen{
				preview:   terminalstate.Preview{Directory: test.terminalDirectory},
				onPreview: func() { order = append(order, "terminal") },
			}
			w := &Worker{
				screen: screen,
				now:    func() time.Time { return observedAt },
				observeProcess: func(int, int) processObservation {
					order = append(order, "process")
					return processObservation{directory: test.processDirectory}
				},
			}

			inspection := w.inspectSession(80, 6)
			if inspection.CurrentDirectory != test.wantDirectory || inspection.DirectorySource != test.wantSource {
				t.Fatalf("inspection directory = %q (%q), want %q (%q)", inspection.CurrentDirectory, inspection.DirectorySource, test.wantDirectory, test.wantSource)
			}
			if strings.Join(order, ",") != "process,terminal" {
				t.Fatalf("observation order = %v, want process then terminal", order)
			}
		})
	}
}

func TestPumpReportsLastOutputActivityToInspection(t *testing.T) {
	sid, err := protocol.NewSessionID("LIVE")
	if err != nil {
		t.Fatal(err)
	}
	outputAt := time.Date(2026, time.September, 4, 9, 2, 0, 0, time.UTC)
	pty := newPipePTY()
	w := &Worker{
		cfg:      Config{ID: sid.String()},
		sid:      sid,
		pty:      pty,
		ring:     session.NewRing(ringSize),
		screen:   &recordingScreen{},
		pumpDone: make(chan struct{}),
		now:      func() time.Time { return outputAt },
		observeProcess: func(int, int) processObservation {
			return processObservation{}
		},
	}
	go w.pump()
	defer func() {
		_ = pty.Close()
		<-w.pumpDone
	}()

	if _, err := pty.emit([]byte("working\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.lastOutputAt.Equal(outputAt)
	})

	response := inspectRequest(t, w, protocol.Control{
		Type:        protocol.TypeInspect,
		RequestID:   "inspect-activity",
		SessionID:   sid.String(),
		PreviewCols: 80,
		PreviewRows: 6,
	})
	if response.Type != protocol.TypeInspected || response.Inspection == nil || response.Inspection.LastOutputAt == nil || !response.Inspection.LastOutputAt.Equal(outputAt) {
		t.Fatalf("inspection activity response = %+v", response)
	}
}

func TestInspectRejectsInvalidDimensionsAsProtocolError(t *testing.T) {
	tests := []struct {
		name string
		cols int
		rows int
	}{
		{name: "missing columns", rows: 1},
		{name: "missing rows", cols: 1},
		{name: "too many columns", cols: protocol.MaxInspectionPreviewCols + 1, rows: 1},
		{name: "too many rows", cols: 1, rows: protocol.MaxInspectionPreviewRows + 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observerCalls := 0
			screen := &recordingScreen{}
			active := &attachment{}
			w := &Worker{
				cfg:    Config{ID: "7K3D"},
				screen: screen,
				client: active,
				observeProcess: func(int, int) processObservation {
					observerCalls++
					return processObservation{}
				},
			}
			response := inspectRequest(t, w, protocol.Control{
				Type:        protocol.TypeInspect,
				RequestID:   "invalid-dimensions",
				SessionID:   "7K3D",
				PreviewCols: test.cols,
				PreviewRows: test.rows,
			})

			if response.Type != protocol.TypeError || response.RequestID != "invalid-dimensions" || response.SessionID != "7K3D" || !strings.Contains(response.Message, "protocol: inspection dimensions") {
				t.Fatalf("invalid dimensions response = %+v", response)
			}
			if response.Inspection != nil || observerCalls != 0 || screen.previewCalls != 0 {
				t.Fatalf("invalid request was inspected: response %+v, observer calls %d, preview calls %d", response, observerCalls, screen.previewCalls)
			}
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.client != active {
				t.Fatal("invalid inspection displaced the active attacher")
			}
		})
	}
}

func TestInspectValidatesItsProtocolResponse(t *testing.T) {
	observedAt := time.Date(2026, time.September, 4, 9, 3, 0, 0, time.UTC)
	w := &Worker{
		cfg:    Config{ID: "7K3D"},
		screen: &recordingScreen{preview: terminalstate.Preview{Lines: []string{"unsafe\nline"}}},
		now:    func() time.Time { return observedAt },
		observeProcess: func(int, int) processObservation {
			return processObservation{}
		},
	}
	response := inspectRequest(t, w, protocol.Control{
		Type:        protocol.TypeInspect,
		RequestID:   "invalid-response",
		SessionID:   "7K3D",
		PreviewCols: 80,
		PreviewRows: 6,
	})
	if response.Type != protocol.TypeError || response.Inspection != nil || !strings.Contains(response.Message, "protocol: inspection preview") {
		t.Fatalf("invalid inspection response = %+v", response)
	}
}

func TestInspectionBoundsAggregateUnicodeText(t *testing.T) {
	observedAt := time.Date(2026, time.September, 4, 9, 4, 0, 0, time.UTC)
	w := &Worker{
		screen: &recordingScreen{preview: terminalstate.Preview{
			Lines:     []string{"e" + strings.Repeat("\u0301", protocol.MaxInspectionTextBytes)},
			Title:     strings.Repeat("title", 800),
			Directory: "/terminal/current",
		}},
		now: func() time.Time { return observedAt },
		observeProcess: func(int, int) processObservation {
			return processObservation{directory: "/process/current", command: "editor"}
		},
	}

	inspection := w.inspectSession(protocol.MaxInspectionPreviewCols, protocol.MaxInspectionPreviewRows)
	if err := protocol.ValidateSessionInspection(inspection); err != nil {
		t.Fatalf("bounded inspection failed protocol validation: %v", err)
	}
	if inspection.CurrentDirectory != "/process/current" || inspection.ForegroundCommand != "editor" || inspection.TerminalTitle == "" {
		t.Fatalf("bounding discarded high-priority metadata: %+v", inspection)
	}
}

func TestInspectionKeepsWireTimesOrderedAcrossClockRollback(t *testing.T) {
	lastOutputAt := time.Date(2026, time.September, 4, 9, 5, 0, 0, time.UTC)
	observedAt := lastOutputAt.Add(-time.Minute)
	w := &Worker{
		screen:       &recordingScreen{},
		lastOutputAt: lastOutputAt,
		now:          func() time.Time { return observedAt },
		observeProcess: func(int, int) processObservation {
			return processObservation{}
		},
	}

	inspection := w.inspectSession(80, 6)
	if inspection.LastOutputAt == nil || !inspection.LastOutputAt.Equal(observedAt) {
		t.Fatalf("last output = %v, want clamped observation time %v", inspection.LastOutputAt, observedAt)
	}
	if err := protocol.ValidateSessionInspection(inspection); err != nil {
		t.Fatalf("clock-adjusted inspection failed wire validation: %v", err)
	}
	payload, err := (protocol.Control{Type: protocol.TypeInspected, Inspection: &inspection}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := protocol.DecodeControl(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.ValidateSessionInspection(*decoded.Inspection); err != nil {
		t.Fatalf("clock-adjusted inspection failed after wire round trip: %v", err)
	}
}

func TestProcessObservationCrossingReapIsDiscarded(t *testing.T) {
	observerStarted := make(chan struct{})
	releaseObserver := make(chan struct{})
	var startedOnce sync.Once
	w := &Worker{
		cmd: &exec.Cmd{Process: &os.Process{Pid: 4321}},
		observeProcess: func(int, int) processObservation {
			startedOnce.Do(func() { close(observerStarted) })
			<-releaseObserver
			return processObservation{directory: "/unrelated", command: "not-our-process"}
		},
	}

	result := make(chan processObservation, 1)
	go func() { result <- w.currentProcessObservation() }()
	select {
	case <-observerStarted:
	case <-time.After(time.Second):
		t.Fatal("process observer did not start")
	}
	w.mu.Lock()
	w.reaped = true
	w.mu.Unlock()
	close(releaseObserver)

	select {
	case observation := <-result:
		if observation != (processObservation{}) {
			t.Fatalf("observation crossing reap = %+v, want empty", observation)
		}
	case <-time.After(time.Second):
		t.Fatal("process observer did not return")
	}
}

func inspectRequest(t *testing.T, w *Worker, request protocol.Control) protocol.Control {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close() //nolint:errcheck // test resource cleanup
	go w.serve(server)
	if err := protocol.NewWriter(client).WriteControlMsg(request); err != nil {
		t.Fatal(err)
	}
	frame, err := protocol.NewReader(client).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Kind != protocol.KindControl {
		t.Fatalf("inspection frame kind = %v, want control", frame.Kind)
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
