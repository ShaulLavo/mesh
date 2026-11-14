package updatebootstrap

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/worker"
)

func TestWorkerProbeAcceptsLogTailAndExactLegacyAcknowledgement(t *testing.T) {
	inspection := protocol.SessionInspection{ObservedAt: time.Now()}
	tests := []struct {
		name     string
		meta     worker.Meta
		response protocol.Control
		want     bool
	}{
		{
			name:     "modern log tail",
			meta:     worker.Meta{ID: "7K3D", Build: &release.Build{}},
			response: protocol.Control{Type: protocol.TypeLogged, RequestID: "bootstrap-worker", SessionID: "7K3D", Output: []byte("$")},
			want:     true,
		},
		{
			name:     "log tail of a session with no output",
			meta:     worker.Meta{ID: "7K3D"},
			response: protocol.Control{Type: protocol.TypeLogged, RequestID: "bootstrap-worker", SessionID: "7K3D"},
			want:     true,
		},
		{
			name:     "legacy pre-attach acknowledgement",
			meta:     worker.Meta{ID: "7K3D"},
			response: protocol.Control{Type: protocol.TypeError, SessionID: "7K3D", Message: "expected " + protocol.TypeAttach},
			want:     true,
		},
		{
			name:     "modern worker cannot downgrade",
			meta:     worker.Meta{ID: "7K3D", Build: &release.Build{}},
			response: protocol.Control{Type: protocol.TypeError, SessionID: "7K3D", Message: "expected " + protocol.TypeAttach},
		},
		{
			name:     "wrong legacy session",
			meta:     worker.Meta{ID: "7K3D"},
			response: protocol.Control{Type: protocol.TypeError, SessionID: "OTHER", Message: "expected " + protocol.TypeAttach},
		},
		{
			name:     "legacy request id not echoed",
			meta:     worker.Meta{ID: "7K3D"},
			response: protocol.Control{Type: protocol.TypeError, RequestID: "bootstrap-worker", SessionID: "7K3D", Message: "expected " + protocol.TypeAttach},
		},
		{
			name:     "different legacy error",
			meta:     worker.Meta{ID: "7K3D"},
			response: protocol.Control{Type: protocol.TypeError, SessionID: "7K3D", Message: "expected session.attach-detached"},
		},
		{
			name:     "log tail requires exact session",
			meta:     worker.Meta{ID: "7K3D"},
			response: protocol.Control{Type: protocol.TypeLogged, RequestID: "bootstrap-worker", SessionID: "OTHER"},
		},
		{
			name:     "log tail requires exact request",
			meta:     worker.Meta{ID: "7K3D"},
			response: protocol.Control{Type: protocol.TypeLogged, RequestID: "other-request", SessionID: "7K3D"},
		},
		{
			name:     "refused log tail",
			meta:     worker.Meta{ID: "7K3D", Build: &release.Build{}},
			response: protocol.Control{Type: protocol.TypeError, RequestID: "bootstrap-worker", SessionID: "7K3D", Message: "log tail must be between 1 and 1048576 bytes"},
		},
		{
			name:     "answer to another request",
			meta:     worker.Meta{ID: "7K3D", Build: &release.Build{}},
			response: protocol.Control{Type: protocol.TypeInspected, RequestID: "bootstrap-worker", SessionID: "7K3D", Inspection: &inspection},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validWorkerProbe(test.response, test.meta); got != test.want {
				t.Fatalf("validWorkerProbe() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestWorkerProbeSkipsKilledWorker(t *testing.T) {
	dir := filepath.Join(probeFixtureDir(t), "7K3D")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteMeta(dir, worker.Meta{ID: "7K3D", State: worker.StateRunning, BootID: worker.BootID()}); err != nil {
		t.Fatal(err)
	}
	// A socket file with no listener is what SIGKILL leaves behind.
	listener, err := net.Listen("unix", filepath.Join(dir, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = listener.Close()
	current, err := inspectWorker(t.Context(), dir, 1)
	if err != nil || current != nil {
		t.Fatalf("inspectWorker = %+v, %v; want nothing to preserve", current, err)
	}
}
