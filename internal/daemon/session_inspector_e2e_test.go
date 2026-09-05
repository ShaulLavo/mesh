package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

const sessionInspectorE2ETimeout = 5 * time.Second

func TestSessionInspectionTraversesWebSocketDaemonAndLiveWorker(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	sessionDir := filepath.Join(sessionsDir, "7K3D")
	currentDirectory := filepath.Join(root, "current-project")
	for _, directory := range []string{sessionDir, currentDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	canonicalCurrentDirectory, err := filepath.EvalSymlinks(currentDirectory)
	if err != nil {
		t.Fatal(err)
	}

	probeTerminalSize := filepath.Join(root, "probe-terminal-size")
	stopCommand := filepath.Join(root, "stop-command")
	command := []string{
		"/bin/sh", "-c", `
cd "$1" || exit 91
printf '\033]2;mesh-inspection-e2e\007'
printf '\033[1;32mMESH_INSPECTION_E2E\033[0m\r\n'
while [ ! -e "$2" ]; do sleep 0.01; done
stty size
while [ ! -e "$3" ]; do sleep 0.01; done
`,
		"mesh-inspection-e2e", currentDirectory, probeTerminalSize, stopCommand,
	}
	type workerResult struct {
		code int
		err  error
	}
	workerDone := make(chan workerResult, 1)
	go func() {
		code, err := worker.Run(worker.Config{
			ID:      "7K3D",
			Dir:     sessionDir,
			Command: command,
			Cwd:     root,
			Env:     []string{"PATH=/usr/bin:/bin", "TERM=xterm-256color", "LANG=C"},
			Cols:    91,
			Rows:    33,
		})
		workerDone <- workerResult{code: code, err: err}
	}()

	var (
		stopOnce     sync.Once
		stopResult   workerResult
		stopTimedOut bool
		stopWriteErr error
	)
	stopWorker := func() {
		stopOnce.Do(func() {
			stopWriteErr = os.WriteFile(stopCommand, nil, 0o600)
			if stopWriteErr != nil {
				return
			}
			select {
			case stopResult = <-workerDone:
			case <-time.After(sessionInspectorE2ETimeout):
				stopTimedOut = true
			}
		})
	}
	t.Cleanup(func() {
		stopWorker()
		if stopWriteErr != nil {
			t.Errorf("stop live worker: %v", stopWriteErr)
		} else if stopTimedOut {
			t.Error("live worker did not stop")
		} else if stopResult.err != nil || stopResult.code != 0 {
			t.Errorf("live worker result = code %d, error %v", stopResult.code, stopResult.err)
		}
	})
	waitForPath(t, paths.Socket(sessionDir))

	catalog := &lifecycleTestCatalog{sessions: []storage.Session{{
		ID:        "7K3D",
		HostID:    "host-a",
		Command:   append([]string(nil), command...),
		Cwd:       root,
		State:     storage.StateDetached,
		CreatedAt: time.Now(),
	}}}
	connector, err := newWorkerConnector(sessionsDir, catalog)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog:     catalog,
		Connector:   connector,
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: sessionsDir,
	})
	server, err := newClientServer(lifecycle, connector, disabledEdgeController{}, noServiceControl{}, disabledCertificateController{})
	if err != nil {
		t.Fatal(err)
	}

	webSocketServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/mesh" {
			http.NotFound(response, request)
			return
		}
		_ = transport.Serve(response, request, server.Handle)
	}))
	defer webSocketServer.Close()

	dialContext, cancelDial := context.WithTimeout(context.Background(), sessionInspectorE2ETimeout)
	client, err := transport.DialOnce(
		dialContext,
		"ws"+strings.TrimPrefix(webSocketServer.URL, "http")+"/mesh",
		transport.DialOptions{},
	)
	cancelDial()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close() //nolint:errcheck // test connection cleanup

	inspection := waitForLiveSessionInspection(t, client, func(got protocol.SessionInspection) bool {
		observedDirectory, resolveErr := filepath.EvalSymlinks(got.CurrentDirectory)
		return resolveErr == nil && observedDirectory == canonicalCurrentDirectory &&
			got.DirectorySource == protocol.DirectorySourceProcess &&
			got.TerminalTitle == "mesh-inspection-e2e" &&
			strings.Contains(strings.Join(got.Preview, "\n"), "MESH_INSPECTION_E2E")
	})
	if inspection.Attached {
		t.Fatal("read-only inspection acquired an attachment")
	}
	if !inspectionHasStyledText(inspection, "MESH_INSPECTION_E2E", func(style protocol.PreviewStyle) bool {
		return style.Bold && style.Foreground == (protocol.PreviewColor{Kind: protocol.PreviewColorBasic, Value: 2})
	}) {
		t.Fatalf("styled marker did not survive worker, daemon, and WebSocket: %+v", inspection.StyledPreview)
	}

	if err := os.WriteFile(probeTerminalSize, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	inspection = waitForLiveSessionInspection(t, client, func(got protocol.SessionInspection) bool {
		return strings.Contains(strings.Join(got.Preview, "\n"), "33 91")
	})
	if inspection.Attached {
		t.Fatal("repeated read-only inspection acquired an attachment")
	}
	observedDirectory, err := filepath.EvalSymlinks(inspection.CurrentDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if observedDirectory != canonicalCurrentDirectory || inspection.TerminalTitle != "mesh-inspection-e2e" {
		t.Fatalf("inspection lost live context: %+v", inspection)
	}

	stopWorker()
	if stopWriteErr != nil {
		t.Fatal(stopWriteErr)
	}
	if stopTimedOut {
		t.Fatal("live worker did not stop")
	}
	if stopResult.err != nil || stopResult.code != 0 {
		t.Fatalf("live worker result = code %d, error %v", stopResult.code, stopResult.err)
	}
}

func inspectionHasStyledText(
	inspection protocol.SessionInspection,
	text string,
	matches func(protocol.PreviewStyle) bool,
) bool {
	for _, line := range inspection.StyledPreview {
		for _, run := range line.Runs {
			if strings.Contains(run.Text, text) && matches(run.Style) {
				return true
			}
		}
	}
	return false
}

func waitForLiveSessionInspection(
	t *testing.T,
	client transport.Conn,
	ready func(protocol.SessionInspection) bool,
) protocol.SessionInspection {
	t.Helper()
	deadline := time.Now().Add(sessionInspectorE2ETimeout)
	var last protocol.SessionInspection
	for attempt := 1; ; attempt++ {
		requestID := fmt.Sprintf("inspect-e2e-%d", attempt)
		message := sessionInspectorE2EControlRoundTrip(t, client, protocol.Control{
			Type:        protocol.TypeInspect,
			RequestID:   requestID,
			SessionID:   "7K3D",
			PreviewCols: 40,
			PreviewRows: 6,
		})
		if message.Type != protocol.TypeInspected || message.RequestID != requestID || message.SessionID != "7K3D" || message.Inspection == nil {
			t.Fatalf("inspection response = %+v", message)
		}
		last = *message.Inspection
		if ready(last) {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("live inspection did not become ready; last observation = %+v", last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func sessionInspectorE2EControlRoundTrip(t *testing.T, client transport.Conn, request protocol.Control) protocol.Control {
	t.Helper()
	payload, err := request.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		t.Fatal(err)
	}

	type readResult struct {
		frame protocol.Frame
		err   error
	}
	result := make(chan readResult, 1)
	go func() {
		frame, readErr := client.ReadFrame()
		result <- readResult{frame: frame, err: readErr}
	}()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.frame.Kind != protocol.KindControl {
			t.Fatalf("response frame kind = %d, want control", got.frame.Kind)
		}
		message, decodeErr := protocol.DecodeControl(got.frame.Payload)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		return message
	case <-time.After(sessionInspectorE2ETimeout):
		_ = client.Close()
		t.Fatal("timed out waiting for inspection response")
		return protocol.Control{}
	}
}
