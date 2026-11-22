package agentapi

const (
	ServiceNotFound    Code = "service.not_found"
	MCPMessageTooLarge Code = "mcp.message_too_large"
)

func init() {
	Codes[ServiceNotFound] = ErrorClass{Exit: 1}
	Codes[MCPMessageTooLarge] = ErrorClass{Exit: 1}
}

// HostCapabilities reports optional host protocol features observed in host.info.
type HostCapabilities struct {
	ExecutionPipe    bool `json:"executionPipe"`
	ExecutionContext bool `json:"executionContext"`
	Files            bool `json:"files"`
	MCPHosting       bool `json:"mcpHosting"`
}
