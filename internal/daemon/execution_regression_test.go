package daemon

import (
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/worker"
	"testing"
)

func TestBoundExecutionMismatchKeepsItsPublicCode(t *testing.T) {
	response := clientRequestError(protocol.Control{Type: protocol.TypeExecutionSignal, RequestID: "wrong-instance"}, execution.ErrInstance)
	if response.ErrorCode != "session.instance_mismatch" {
		t.Fatalf("exact-instance code lost: %+v", response)
	}
}
func TestBoundExecutionLostAcknowledgementKeepsUnknownDelivery(t *testing.T) {
	ref, err := session.NewReservation("7K3D", worker.BootID(), session.Execution)
	if err != nil {
		t.Fatal(err)
	}
	r := execution.Receipt{Binding: execution.Binding{Host: "host", Reservation: ref}, Phase: "live"}
	l := &lifecycle{sessionsDir: t.TempDir()}
	request := protocol.Control{Type: protocol.TypeExecutionSignal, RequestID: "no-ack", SessionID: ref.ID, Instance: ref.Instance, Signal: "term"}
	observed, err := l.boundExecution(t.Context(), request, nil, r)
	if err == nil || observed.ExecutionDelivery != "unknown" {
		t.Fatal("delivery uncertainty control did not arm", observed, err)
	}
	response := clientRequestError(request, executionFailure(observed, err))
	if response.ExecutionDelivery != "unknown" {
		t.Fatalf("delivery uncertainty dropped: %+v", response)
	}
}
