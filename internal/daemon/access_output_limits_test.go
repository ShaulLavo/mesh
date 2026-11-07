package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
)

func TestScopedPreviewEncodedLimitUsesResourceLimit(t *testing.T) {
	inspection := denseScopedPreview()
	raw, err := json.Marshal(inspection)
	if err != nil || len(raw) <= protocol.MaximumAccessReply {
		t.Fatalf("encoded limit fixture: size=%d error=%v", len(raw), err)
	}
	assertScopedPreviewResourceLimit(t, inspection)
}

func TestScopedPreviewTextLimitUsesResourceLimit(t *testing.T) {
	line := strings.Repeat("x\u0301\u0301\u0301", 160)
	inspection := protocol.SessionInspection{ObservedAt: time.Now().UTC(), Preview: make([]string, 24), StyledPreview: make([]protocol.PreviewLine, 24)}
	for row := range inspection.Preview {
		inspection.Preview[row] = line
		inspection.StyledPreview[row].Runs = []protocol.PreviewRun{{Text: line}}
	}
	if 2*len(line)*len(inspection.Preview) <= protocol.MaxInspectionTextBytes {
		t.Fatal("fixture does not exceed aggregate text budget")
	}
	assertScopedPreviewResourceLimit(t, inspection)
}

func assertScopedPreviewResourceLimit(t *testing.T, inspection protocol.SessionInspection) {
	t.Helper()
	if err := protocol.ValidateSessionInspection(inspection); err != nil {
		t.Fatalf("valid worker preview required: %v", err)
	}
	f := newScopedFixture(t.Context(), t, false)
	enableScopedOutput(t, f)
	prepareOutputFiles(t, f, storage.StateDetached, []string{"saved"}, nil)
	startScopedReadWorker(t, f, func(request protocol.Control) protocol.Control {
		return protocol.Control{Type: protocol.TypeInspected, RequestID: request.RequestID, SessionID: request.SessionID, Inspection: &inspection}
	})
	reply := scopedOutput(t, f, protocol.AccessOutput{SessionID: "7K3D", Format: "preview", Cols: 160, Rows: 24})
	if reply.Result != nil || reply.ErrorCode != "resource.limit" || reply.Message != "Session output exceeds the response limit." {
		t.Fatalf("oversized valid preview must return sanitized resource limit: %+v", reply)
	}
}

func denseScopedPreview() protocol.SessionInspection {
	inspection := protocol.SessionInspection{ObservedAt: time.Now().UTC(), Preview: make([]string, 24), StyledPreview: make([]protocol.PreviewLine, 24)}
	for row := range inspection.Preview {
		inspection.Preview[row] = strings.Repeat("x", 160)
		inspection.StyledPreview[row].Runs = denseScopedPreviewRuns()
	}
	return inspection
}

func denseScopedPreviewRuns() []protocol.PreviewRun {
	runs := make([]protocol.PreviewRun, 160)
	for i := range runs {
		color := protocol.PreviewColor{Kind: protocol.PreviewColorRGB, Value: uint32(0x123456 + i%2)}
		runs[i] = protocol.PreviewRun{Text: "x", Style: protocol.PreviewStyle{Foreground: color, Background: color, UnderlineColor: color, Bold: true, Faint: true, Italic: true, Reverse: true, Strikethrough: true, Underline: protocol.PreviewUnderlineSingle}}
	}
	return runs
}
