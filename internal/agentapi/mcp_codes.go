package agentapi

const (
	ServiceNotFound     Code = "service.not_found"
	ServiceIncompatible Code = "service.incompatible"
	ServiceUnavailable  Code = "service.unavailable"
	ServiceFailed       Code = "service.failed"
	ServiceConflict     Code = "service.conflict"
	CatalogStale        Code = "catalog.stale"
	MCPStateInvalid     Code = "mcp.state_invalid"
	MCPMessageTooLarge  Code = "mcp.message_too_large"
)

func init() {
	Codes[ServiceNotFound] = ErrorClass{Exit: 1}
	Codes[ServiceIncompatible] = ErrorClass{Exit: 1}
	Codes[ServiceUnavailable] = ErrorClass{Exit: 1, Retryable: true}
	Codes[ServiceFailed] = ErrorClass{Exit: 1}
	Codes[ServiceConflict] = ErrorClass{Exit: 1}
	Codes[CatalogStale] = ErrorClass{Exit: 1, Retryable: true}
	Codes[MCPStateInvalid] = ErrorClass{Exit: 1}
	Codes[MCPMessageTooLarge] = ErrorClass{Exit: 1}
}

// HostCapabilities reports optional host protocol features observed in host.info.
type HostCapabilities struct {
	ExecutionPipe    bool `json:"executionPipe"`
	ExecutionContext bool `json:"executionContext"`
	Files            bool `json:"files"`
	MCPHosting       bool `json:"mcpHosting"`
}
