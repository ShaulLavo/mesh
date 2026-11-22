package mcpconnect

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
)

const (
	// MetaPrefix is the reverse-DNS prefix for Mesh-owned MCP _meta keys.
	MetaPrefix = "com.github.shaullavo.mesh/"
	// MetaUpstreamServerInfo preserves the hosted server's own implementation info.
	MetaUpstreamServerInfo = MetaPrefix + "upstreamServerInfo"

	keyResult            = "result"
	keyError             = "error"
	keyMeta              = "_meta"
	keyParams            = "params"
	keyResultType        = "resultType"
	keyCacheScope        = "cacheScope"
	keyTTLMS             = "ttlMs"
	keySupportedVersions = "supportedVersions"
)

var (
	errNotJSONObject      = errors.New("mcp connect: expected a JSON object")
	rawPrivateCacheScope  = json.RawMessage(`"private"`)
	rawPinnedVersions     = json.RawMessage(`["` + mcpwire.Version + `"]`)
	rawResultTypeComplete = json.RawMessage(`"complete"`)
	rawResultTypeInputReq = json.RawMessage(`"input_required"`)
)

// rewriteForwardedResult normalizes a forwarded terminal MCP result according to
// Mesh's frontend contract (PLAN §2.2, §2.6, §6.6):
//   - advertises Mesh in io.modelcontextprotocol/serverInfo while preserving the
//     upstream identity in com.github.shaullavo.mesh/upstreamServerInfo,
//   - pins server/discover supportedVersions to ["2026-07-28"],
//   - forces cacheScope="private" on complete cacheable replies,
//   - strips cache hints (ttlMs/cacheScope) on input_required continuations,
//   - preserves all other JSON fields and unknown vendor extensions byte-exact.
func rewriteForwardedResult(method string, raw []byte, meshServerInfo, fallbackUpstream json.RawMessage) ([]byte, json.RawMessage, error) {
	top, resultFields, ok := extractResultFields(raw)
	if !ok {
		return raw, nil, nil
	}
	meta, err := decodeOptionalMeta(resultFields[keyMeta])
	if err != nil {
		return nil, nil, err
	}
	observed := applyServerInfoMeta(meta, meshServerInfo, fallbackUpstream)
	encodedMeta, err := encodeCompactJSON(meta)
	if err != nil {
		return nil, nil, fmt.Errorf("encode result _meta: %w", err)
	}
	resultFields[keyMeta] = encodedMeta

	normalizeResultPolicy(method, resultFields)

	encodedResult, err := encodeCompactJSON(resultFields)
	if err != nil {
		return nil, nil, fmt.Errorf("encode result: %w", err)
	}
	top[keyResult] = encodedResult
	updated, err := encodeCompactJSON(top)
	if err != nil {
		return nil, nil, fmt.Errorf("encode response: %w", err)
	}
	if len(updated) > mcpwire.MaxMessage {
		return nil, observed, mcpwire.ErrTooLarge
	}
	return updated, observed, nil
}

func normalizeResultPolicy(method string, resultFields map[string]json.RawMessage) {
	if method == mcpwire.MethodDiscover {
		resultFields[keySupportedVersions] = rawPinnedVersions
	}
	rt := bytes.TrimSpace(resultFields[keyResultType])
	if bytes.Equal(rt, rawResultTypeInputReq) {
		delete(resultFields, keyTTLMS)
		delete(resultFields, keyCacheScope)
		return
	}
	if isCacheableMethod(method) || len(bytes.TrimSpace(resultFields[keyCacheScope])) > 0 {
		resultFields[keyCacheScope] = rawPrivateCacheScope
	}
}

func isCacheableMethod(method string) bool {
	switch method {
	case mcpwire.MethodDiscover, mcpwire.MethodListTools, mcpwire.MethodListPrompts,
		mcpwire.MethodListResources, mcpwire.MethodListResourceTemplates:
		return true
	default:
		return false
	}
}

type hostMeshError struct {
	Code      agentapi.Code
	Retryable bool
	Host      string
	Service   string
	Message   string
}

func parseHostMeshError(raw []byte) (*hostMeshError, bool) {
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    *struct {
				MeshCode  string `json:"meshCode"`
				Retryable bool   `json:"retryable"`
				Host      string `json:"host"`
				Service   string `json:"service"`
			} `json:"data"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Error == nil {
		return nil, false
	}
	if env.Error.Code != meshRPCErrorCode || env.Error.Data == nil || strings.TrimSpace(env.Error.Data.MeshCode) == "" {
		return nil, false
	}
	return &hostMeshError{
		Code:      agentapi.Code(env.Error.Data.MeshCode),
		Retryable: env.Error.Data.Retryable,
		Host:      env.Error.Data.Host,
		Service:   env.Error.Data.Service,
		Message:   env.Error.Message,
	}, true
}

// translateHostMeshToolError converts a host-originated Mesh JSON-RPC error
// (-31000 carrying data.meshCode) on tools/call into the PLAN §2.4 tool-error
// result shape (isError: true with the standard agentapi.Envelope). Upstream
// MCP errors (any other code or missing meshCode) are preserved verbatim.
func translateHostMeshToolError(req mcpwire.Message, raw []byte, fallbackHost, fallbackService string, meshServerInfo, fallbackUpstream json.RawMessage) ([]byte, bool, error) {
	if req.Method() != mcpwire.MethodCallTool {
		return nil, false, nil
	}
	parsed, ok := parseHostMeshError(raw)
	if !ok {
		return nil, false, nil
	}
	host := parsed.Host
	if host == "" {
		host = fallbackHost
	}
	service := parsed.Service
	if service == "" {
		service = fallbackService
	}
	converted, err := buildToolErrorResult(req.ID(), parsed.Code, parsed.Retryable, host, service, parsed.Message, meshServerInfo, fallbackUpstream)
	if err != nil {
		return nil, false, err
	}
	return converted, true, nil
}

// buildToolErrorResult builds the PLAN §2.4 tool-execution error result
// (`isError: true` + `structuredContent` agent envelope + explanatory text) for
// Mesh forwarding failures inside `tools/call`.
func buildToolErrorResult(id mcpwire.ID, code agentapi.Code, retryable bool, hostID, service, message string, meshServerInfo, fallbackUpstream json.RawMessage) ([]byte, error) {
	reqID, err := agentapi.NewRequestID()
	if err != nil {
		return nil, fmt.Errorf("create tool error request ID: %w", err)
	}
	clean := agentapi.CleanMessage(message)
	if clean == "" {
		clean = string(code)
	}
	target := service
	if hostID != "" && service != "" {
		target = hostID + "/" + service
	} else if hostID != "" {
		target = hostID
	}
	envelope := agentapi.Envelope{
		SchemaVersion: agentapi.SchemaVersion,
		Operation:     "mcp.connect",
		Status:        agentapi.StatusError,
		RequestID:     reqID,
		Errors: []agentapi.Error{{
			Code:      code,
			Message:   clean,
			Operation: "mcp.connect",
			Target:    target,
			Retryable: retryable,
		}},
	}
	text := fmt.Sprintf("[%s] %s", code, clean)
	if hint := toolErrorNextHint(code); hint != "" {
		text += " Next: " + hint
	}
	meta := map[string]json.RawMessage{}
	if len(meshServerInfo) > 0 {
		meta[mcpwire.ServerInfoKey] = meshServerInfo
	}
	if len(fallbackUpstream) > 0 {
		meta[MetaUpstreamServerInfo] = fallbackUpstream
	}
	result := map[string]any{
		keyResultType:       rawResultTypeComplete,
		"isError":           true,
		"content":           []map[string]string{{"type": "text", "text": text}},
		"structuredContent": envelope,
		keyMeta:             meta,
	}
	reply := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		keyResult: result,
	}
	encoded, err := encodeCompactJSON(reply)
	if err != nil {
		return nil, fmt.Errorf("encode tool error result: %w", err)
	}
	if len(encoded) > mcpwire.MaxMessage {
		return nil, mcpwire.ErrTooLarge
	}
	return encoded, nil
}

var toolErrorHints = map[agentapi.Code]string{
	agentapi.RequestOutcomeUnknown:    "Check state on the owning host before deciding whether to retry; do not replay blindly.",
	agentapi.HostUnreachable:          "Verify the owning host is online and reachable via Mesh.",
	agentapi.HostIdentityMismatch:     "Verify the host identity pin before retrying.",
	agentapi.AccessDenied:             "Check device approval and application identity authorization on the owning host.",
	agentapi.HostAuthenticationFailed: "Check device approval and application identity authorization on the owning host.",
	agentapi.ServiceNotFound:          "Run mesh mcp ls to inspect available hosted MCP servers.",
	agentapi.ServiceIncompatible:      "The hosted server does not speak stateless MCP 2026-07-28; inspect mesh mcp logs.",
	agentapi.ServiceUnavailable:       "Inspect the hosted instance with mesh mcp show or mesh mcp logs.",
	agentapi.ServiceFailed:            "Inspect the hosted instance with mesh mcp show or mesh mcp logs.",
	agentapi.ResourceLimit:            "Retry after active requests finish or reduce concurrent load.",
	agentapi.MCPMessageTooLarge:       "Reduce the response payload below the 3.5 MiB MCP message limit.",
	agentapi.ProtocolUnsupported:      "Update the owning host daemon to a version with MCP hosting support.",
}

func toolErrorNextHint(code agentapi.Code) string {
	return toolErrorHints[code]
}

func extractResultFields(raw []byte) (top, resultFields map[string]json.RawMessage, ok bool) {
	top, err := decodeObject(raw)
	if err != nil {
		return nil, nil, false
	}
	resultRaw, exists := top[keyResult]
	if !exists || len(bytes.TrimSpace(resultRaw)) == 0 {
		return nil, nil, false
	}
	resultFields, err = decodeObject(resultRaw)
	if err != nil {
		return nil, nil, false
	}
	return top, resultFields, true
}

func decodeOptionalMeta(existing json.RawMessage) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(existing)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	meta, err := decodeObject(existing)
	if err != nil {
		return nil, fmt.Errorf("decode result _meta: %w", err)
	}
	return meta, nil
}

func applyServerInfoMeta(meta map[string]json.RawMessage, meshServerInfo, fallbackUpstream json.RawMessage) json.RawMessage {
	upstream := meta[MetaUpstreamServerInfo]
	if len(upstream) == 0 && len(meta[mcpwire.ServerInfoKey]) > 0 {
		upstream = cloneRaw(meta[mcpwire.ServerInfoKey])
	}
	observed := cloneRaw(upstream)
	if len(upstream) == 0 && len(fallbackUpstream) > 0 {
		upstream = fallbackUpstream
	}
	if len(upstream) > 0 {
		meta[MetaUpstreamServerInfo] = upstream
	}
	if len(meshServerInfo) > 0 {
		meta[mcpwire.ServerInfoKey] = meshServerInfo
	}
	return observed
}

func hasMember(raw []byte, path ...string) bool {
	cur := raw
	for _, name := range path {
		fields, err := decodeObject(cur)
		if err != nil {
			return false
		}
		next, ok := fields[name]
		if !ok || len(bytes.TrimSpace(next)) == 0 || bytes.Equal(bytes.TrimSpace(next), []byte("null")) {
			return false
		}
		cur = next
	}
	return true
}

func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errNotJSONObject
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, fmt.Errorf("mcp connect: decode JSON object: %w", err)
	}
	return fields, nil
}

func encodeCompactJSON(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("mcp connect: encode JSON: %w", err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	out := make(json.RawMessage, len(raw))
	copy(out, raw)
	return out
}
