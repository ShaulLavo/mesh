package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/protocol"
)

type agentContextController struct {
	configDir string
	stateDir  string
	hostID    string
	trusted   func(string) bool
}

func (c *agentContextController) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	if request.Type != protocol.TypeHostContext {
		return protocol.Control{}, false, nil
	}
	if err := ctx.Err(); err != nil {
		return protocol.Control{}, true, fmt.Errorf("host context cancelled: %w", err)
	}
	options, err := hostprofile.ParseContextRequest(request.Context)
	if err != nil {
		return protocol.Control{}, true, fmt.Errorf("host context request: %w", err)
	}
	reply := hostprofile.ReadContext(c.configDir, c.stateDir, c.hostID, c.trusted, options.Full)
	data, err := json.Marshal(reply)
	if err != nil {
		return protocol.Control{}, true, fmt.Errorf("encode host context: %w", err)
	}
	return protocol.Control{Type: protocol.TypeHostContextResult, RequestID: request.RequestID, Context: data}, true, nil
}
