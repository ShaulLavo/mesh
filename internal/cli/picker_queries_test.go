package cli

import (
	"context"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/transport"
)

func TestPickerOperationGateDrainsStartedWorkAndRejectsLateWork(t *testing.T) {
	gate := newPickerOperationGate()
	if !gate.begin(context.Background()) {
		t.Fatal("new gate rejected its first query")
	}

	gate.stop()
	if gate.begin(context.Background()) {
		t.Fatal("stopped gate accepted a late query")
	}
	drained := make(chan struct{})
	go func() {
		gate.wait()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("gate drained before its started query finished")
	default:
	}

	gate.done()
	<-drained
}

func TestPickerReturnCancelsAndDrainsAnInFlightSessionAction(t *testing.T) {
	host := setupCommandTestHost(t)
	started := make(chan struct{})
	finished := make(chan struct{})
	actionResult := make(chan error, 1)
	_, _, err := executeCommand(t, Dependencies{
		DialHost: host.dial,
		DialControl: func(ctx context.Context, _ HostRecord) (transport.Conn, error) {
			close(started)
			<-ctx.Done()
			close(finished)
			return nil, ctx.Err()
		},
		Picker: func(ctx context.Context, input PickerInput) (PickerSelection, error) {
			go func() {
				actionResult <- input.Action(ctx, PickerSessionActionRequest{
					HostAlias: "pc", SessionID: "7K3D", Action: PickerKillSession,
				})
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("session action did not reach the dialer")
			}
			return PickerSelection{}, nil
		},
	}, []string{}...)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("picker returned before the action was canceled and drained")
	}
	if actionErr := <-actionResult; actionErr == nil {
		t.Fatal("canceled action returned no error")
	}
}
