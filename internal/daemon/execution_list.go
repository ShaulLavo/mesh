package daemon

import (
	"context"

	"github.com/shaul/mesh/internal/protocol"
)

func (l *lifecycle) executionListControl(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	key, err := l.executionKey(ctx, "list")
	if err != nil {
		return protocol.Control{}, err
	}
	journal, err := l.executionJournal()
	if err != nil {
		return protocol.Control{}, err
	}
	receipts, err := journal.List(ctx, key.Host, key.Class, key.Principal)
	if err != nil {
		return protocol.Control{}, executionBoundaryError("list execution receipts", err)
	}
	wire := make([]protocol.ExecutionReceipt, 0, len(receipts))
	for _, r := range receipts {
		observed, obsErr := l.observeBoundExecution(ctx, journal, r)
		if obsErr == nil {
			r = observed
		}
		wire = append(wire, *executionWireReceipt(r))
	}
	return protocol.Control{
		Type:              protocol.TypeExecutionListed,
		RequestID:         request.RequestID,
		ExecutionReceipts: wire,
	}, nil
}
