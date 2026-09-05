package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func TestLegacyInspectionRecoversOnlyANSIStylesThatMatchTheWorkerScreen(t *testing.T) {
	sessionsDir := t.TempDir()
	writeLegacyInspectionMeta(t, sessionsDir, 1234)
	observedAt := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	inspectionConn := &lifecycleRecordingConn{readFrame: controlFrame(t, protocol.Control{
		Type:      protocol.TypeInspected,
		RequestID: "inspect-legacy",
		SessionID: "7K3D",
		Inspection: &protocol.SessionInspection{
			ObservedAt: observedAt,
			Preview:    []string{"plain green", "next"},
		},
	})}
	logsConn := &lifecycleRecordingConn{readFrame: controlFrame(t, protocol.Control{
		Type:      protocol.TypeLogged,
		RequestID: "inspect-legacy",
		SessionID: "7K3D",
		Output:    []byte("plain \x1b[32mgreen\x1b[0m\r\nnext"),
	})}
	connections := []transport.Conn{inspectionConn, logsConn}
	connectCalls := 0
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog: &lifecycleTestCatalog{},
		Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
			connection := connections[connectCalls]
			connectCalls++
			return connection, nil
		}),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: sessionsDir,
	})
	lifecycle.observeTerminalSize = func(pid int) (int, int, bool) {
		if pid != 1234 {
			t.Fatalf("terminal-size PID = %d", pid)
		}
		return 20, 2, true
	}

	response, handled, err := lifecycle.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeInspect, RequestID: "inspect-legacy", SessionID: "7K3D", PreviewCols: 20, PreviewRows: 2,
	})
	if err != nil || !handled {
		t.Fatalf("inspection handled = %t, error = %v", handled, err)
	}
	if response.Inspection == nil || len(response.Inspection.StyledPreview) != 2 {
		t.Fatalf("legacy styled inspection = %+v", response.Inspection)
	}
	foundGreen := false
	for _, run := range response.Inspection.StyledPreview[0].Runs {
		if run.Text == "green" && run.Style.Foreground == (protocol.PreviewColor{Kind: protocol.PreviewColorBasic, Value: 2}) {
			foundGreen = true
		}
	}
	if !foundGreen {
		t.Fatalf("source green style missing: %+v", response.Inspection.StyledPreview)
	}
	if connectCalls != 2 {
		t.Fatalf("worker connections = %d, want inspect plus logs", connectCalls)
	}
	loggedRequest, err := protocol.DecodeControl(logsConn.frames[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if loggedRequest.Type != protocol.TypeLogs || loggedRequest.Tail != protocol.MaxLogTail {
		t.Fatalf("legacy replay request = %+v", loggedRequest)
	}
	if err := protocol.ValidateSessionInspection(*response.Inspection); err != nil {
		t.Fatalf("enriched inspection is invalid: %v", err)
	}
}

func TestLegacyInspectionNeverUsesReplayedStylesForAMismatchedScreen(t *testing.T) {
	sessionsDir := t.TempDir()
	writeLegacyInspectionMeta(t, sessionsDir, 1234)
	inspectionConn := &lifecycleRecordingConn{readFrame: controlFrame(t, protocol.Control{
		Type: protocol.TypeInspected, RequestID: "inspect-legacy", SessionID: "7K3D",
		Inspection: &protocol.SessionInspection{
			ObservedAt: time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC),
			Preview:    []string{"authoritative screen"},
		},
	})}
	logsConn := &lifecycleRecordingConn{readFrame: controlFrame(t, protocol.Control{
		Type: protocol.TypeLogged, RequestID: "inspect-legacy", SessionID: "7K3D",
		Output: []byte("\x1b[31mdifferent screen\x1b[0m"),
	})}
	connections := []transport.Conn{inspectionConn, logsConn}
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog: &lifecycleTestCatalog{},
		Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
			connection := connections[0]
			connections = connections[1:]
			return connection, nil
		}),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: sessionsDir,
	})
	lifecycle.observeTerminalSize = func(int) (int, int, bool) { return 40, 1, true }

	response, _, err := lifecycle.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeInspect, RequestID: "inspect-legacy", SessionID: "7K3D", PreviewCols: 40, PreviewRows: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Inspection == nil || len(response.Inspection.StyledPreview) != 0 || response.Inspection.Preview[0] != "authoritative screen" {
		t.Fatalf("mismatched replay changed inspection: %+v", response.Inspection)
	}
}

func TestStyledWorkerInspectionDoesNotRequestLegacyOutput(t *testing.T) {
	inspection := protocol.SessionInspection{
		ObservedAt: time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC),
		Preview:    []string{"green"},
		StyledPreview: []protocol.PreviewLine{{Runs: []protocol.PreviewRun{{
			Text: "green", Style: protocol.PreviewStyle{Foreground: protocol.PreviewColor{Kind: protocol.PreviewColorBasic, Value: 2}},
		}}}},
	}
	connection := &lifecycleRecordingConn{readFrame: controlFrame(t, protocol.Control{
		Type: protocol.TypeInspected, RequestID: "inspect-modern", SessionID: "7K3D", Inspection: &inspection,
	})}
	connectCalls := 0
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog: &lifecycleTestCatalog{},
		Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
			connectCalls++
			return connection, nil
		}),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: t.TempDir(),
	})
	lifecycle.observeTerminalSize = func(int) (int, int, bool) {
		t.Fatal("modern inspection probed legacy terminal size")
		return 0, 0, false
	}

	response, _, err := lifecycle.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeInspect, RequestID: "inspect-modern", SessionID: "7K3D", PreviewCols: 40, PreviewRows: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if connectCalls != 1 || response.Inspection == nil || len(response.Inspection.StyledPreview) != 1 {
		t.Fatalf("modern response = %+v, connections = %d", response, connectCalls)
	}
}

func writeLegacyInspectionMeta(t *testing.T, sessionsDir string, pid int) {
	t.Helper()
	sessionDir := filepath.Join(sessionsDir, "7K3D")
	if err := os.Mkdir(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteMeta(sessionDir, worker.Meta{ID: "7K3D", PID: pid}); err != nil {
		t.Fatal(err)
	}
}
