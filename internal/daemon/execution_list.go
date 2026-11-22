package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
)

func currentDaemonAccount() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

const executionListMaxBytes = 2 << 20

func (l *lifecycle) executionListControl(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if err := validateRequestID(request); err != nil {
		return protocol.Control{}, err
	}
	if request.ExecutionListLimit < 0 || request.ExecutionListLimit > 500 {
		return protocol.Control{}, &protocol.ExecutionValidationError{Err: fmt.Errorf("execution list limit must be between 1 and 500")}
	}
	key, err := l.executionKey(ctx, "list")
	if err != nil {
		return protocol.Control{}, err
	}
	journal, err := l.executionJournal()
	if err != nil {
		return protocol.Control{}, err
	}
	page, err := journal.ListPage(ctx, execution.ListQuery{
		Host:         key.Host,
		Class:        key.Class,
		Principal:    key.Principal,
		IncludeEnded: request.ExecutionListAll,
		Limit:        request.ExecutionListLimit,
		Cursor:       request.Cursor,
		MaxBytes:     executionListMaxBytes,
	}, func(r execution.Receipt) int {
		raw, _ := json.Marshal(executionWireReceipt(r))
		return len(raw) + 2
	})
	if err != nil {
		return protocol.Control{}, executionFailure(protocol.Control{}, executionBoundaryError("list execution receipts", err))
	}
	wire := make([]protocol.ExecutionReceipt, 0, len(page.Receipts))
	for _, r := range page.Receipts {
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
		ExecutionNextPage: page.NextCursor,
	}, nil
}
