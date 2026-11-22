package mcpconnect

import (
	"encoding/json"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
)

// The aggregate gateway forwards to many services through one frontend, so
// it applies the direct frontend's result contract per route and never
// carries one service's identity over to another's result.

// RewriteForwardedResult applies the direct frontend's result contract to one
// forwarded terminal response, without a fallback upstream identity.
func RewriteForwardedResult(method string, raw []byte, meshServerInfo json.RawMessage) ([]byte, error) {
	rewritten, _, err := rewriteForwardedResult(method, raw, meshServerInfo, nil)
	return rewritten, err
}

// TranslateHostToolError turns a host's Mesh JSON-RPC error for tools/call
// into the section 2.4 isError tool result, as the direct frontend does.
func TranslateHostToolError(req mcpwire.Message, raw []byte, host, service string, meshServerInfo json.RawMessage) ([]byte, bool, error) {
	return translateHostMeshToolError(req, raw, host, service, meshServerInfo, nil)
}

// ToolErrorResult builds the section 2.4 isError tool result for a Mesh
// failure inside tools/call.
func ToolErrorResult(id mcpwire.ID, code agentapi.Code, retryable bool, host, service, message string, meshServerInfo json.RawMessage) ([]byte, error) {
	return buildToolErrorResult(id, code, retryable, host, service, message, meshServerInfo, nil)
}
