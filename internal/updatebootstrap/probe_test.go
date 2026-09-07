package updatebootstrap

import (
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/worker"
)

func TestWorkerProbeAcceptsModernInspectionAndExactLegacyAcknowledgement(t *testing.T) {
	inspection := protocol.SessionInspection{ObservedAt: time.Now()}
	tests := []struct {
		name     string
		meta     worker.Meta
		response protocol.Control
		want     bool
	}{
		{
			name:     "modern inspection",
			meta:     worker.Meta{ID: "7K3D", Build: &release.Build{}},
			response: protocol.Control{Type: protocol.TypeInspected, RequestID: "bootstrap-worker", SessionID: "7K3D", Inspection: &inspection},
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
			name:     "inspection requires body",
			meta:     worker.Meta{ID: "7K3D"},
			response: protocol.Control{Type: protocol.TypeInspected, RequestID: "bootstrap-worker", SessionID: "7K3D"},
		},
		{
			name:     "inspection requires exact session",
			meta:     worker.Meta{ID: "7K3D"},
			response: protocol.Control{Type: protocol.TypeInspected, RequestID: "bootstrap-worker", SessionID: "OTHER", Inspection: &inspection},
		},
		{
			name:     "inspection requires exact request",
			meta:     worker.Meta{ID: "7K3D"},
			response: protocol.Control{Type: protocol.TypeInspected, RequestID: "other-request", SessionID: "7K3D", Inspection: &inspection},
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
