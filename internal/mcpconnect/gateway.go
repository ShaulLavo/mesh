package mcpconnect

import "encoding/json"

// RewriteResultServerInfo is the server identity rewrite the direct frontend
// applies, for the aggregate gateway to use on each forwarded result.
func RewriteResultServerInfo(raw []byte, meshServerInfo, fallbackUpstream json.RawMessage) ([]byte, json.RawMessage, error) {
	return rewriteResultServerInfo(raw, meshServerInfo, fallbackUpstream)
}
