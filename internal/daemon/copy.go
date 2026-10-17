package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transfer"
)

// copyController belongs to exactly one authenticated transport connection.
// Its lifetime never creates a terminal session or durable daemon state.
type copyController struct {
	store *transfer.Store
}

func newCopyController() *copyController { return &copyController{store: transfer.NewStore()} }

func (c *copyController) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	if request.Type != protocol.TypeCopyRequest {
		return protocol.Control{}, false, nil
	}
	response := protocol.Control{Type: protocol.TypeCopyResult, RequestID: request.RequestID}
	var operation transfer.Request
	if err := json.Unmarshal(request.Copy, &operation); err != nil {
		_, _ = c.store.Handle(ctx, transfer.Request{Action: "close"})
		return response, true, fmt.Errorf("daemon: decode copy request: %w", err)
	}
	result, err := c.store.Handle(ctx, operation)
	if err != nil {
		return response, true, fmt.Errorf("daemon: copy %s: %w", operation.Action, err)
	}
	response.Copy, err = json.Marshal(result)
	if err != nil {
		return response, true, fmt.Errorf("daemon: encode copy result: %w", err)
	}
	return response, true, nil
}

func (c *copyController) Close() error {
	if err := c.store.Close(); err != nil {
		return fmt.Errorf("daemon: close transfer store: %w", err)
	}
	return nil
}
