package mcphost

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/mcpwire"
)

// RejectedTool is a tools/list entry that must not be advertised over HTTP.
type RejectedTool struct {
	Name   string
	Reason error
}

// ToolHeaders holds the compiled x-mcp-header parameters of every tool in one
// complete tools/list result for one account.
type ToolHeaders struct {
	compiled map[string]mcpwire.ParameterHeaders
	Rejected []RejectedTool
}

// CompileToolHeaders validates the x-mcp-header annotations of each tool in a
// tools/list result (the `result` object, not the envelope) before the tool
// is advertised. A tool whose annotations break the mirroring rules, or whose
// name appears twice, is rejected and left out of Lookup.
func CompileToolHeaders(result json.RawMessage) (ToolHeaders, error) {
	var listing struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &listing); err != nil {
		return ToolHeaders{}, fmt.Errorf("decode MCP tool list: %w", err)
	}
	counts := make(map[string]int, len(listing.Tools))
	for _, tool := range listing.Tools {
		counts[tool.Name]++
	}
	headers := ToolHeaders{compiled: make(map[string]mcpwire.ParameterHeaders, len(listing.Tools))}
	for _, tool := range listing.Tools {
		if counts[tool.Name] > 1 {
			headers.Rejected = append(headers.Rejected, RejectedTool{Name: tool.Name, Reason: errors.New("tool name is listed more than once")})
			continue
		}
		compiled, err := mcpwire.CompileParameterHeaders(tool.InputSchema)
		if err != nil {
			headers.Rejected = append(headers.Rejected, RejectedTool{Name: tool.Name, Reason: err})
			continue
		}
		headers.compiled[tool.Name] = compiled
	}
	return headers, nil
}

// Lookup returns the parameter headers of an advertised tool.
func (h ToolHeaders) Lookup(name string) (mcpwire.ParameterHeaders, bool) {
	compiled, ok := h.compiled[name]
	return compiled, ok
}
