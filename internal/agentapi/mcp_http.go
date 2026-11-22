package agentapi

import "time"

const httpAddrFlag = "--http"

// MCPTokenRotateData describes one completed loopback HTTP bearer token
// rotation without exposing the bearer secret.
type MCPTokenRotateData struct {
	Listener       string    `json:"listener"`
	Endpoint       string    `json:"endpoint"`
	Identity       string    `json:"identity"`
	Generation     uint64    `json:"generation"`
	RetiredStreams int       `json:"retiredStreams"`
	TokenFile      string    `json:"tokenFile"`
	RotatedAt      time.Time `json:"rotatedAt"`
}

var mcpHTTPOperations = []Operation{
	{
		Name:        "mcp.token.rotate",
		Command:     "mesh mcp token rotate",
		Implemented: true,
		Mutates:     true,
		Flags: []Flag{
			{Name: httpAddrFlag, Value: "ADDR", Required: true, Summary: "exact loopback host:port of the running listener"},
			{Name: identityFlag, Value: nameValue, Required: true, Summary: "local application identity bound to the listener"},
			{Name: jsonFlag, Summary: jsonFlagSummary},
		},
		Summary: "Rotate the bearer token for a running loopback MCP HTTP listener and retire streams using the old token.",
		Example: "mesh mcp token rotate --http 127.0.0.1:8765 --identity codex --json",
	},
}

func mcpHTTPValueFlag(arg string) bool {
	return arg == httpAddrFlag
}
