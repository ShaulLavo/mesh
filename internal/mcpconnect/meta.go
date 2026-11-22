package mcpconnect

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/mcpwire"
)

const (
	// MetaPrefix is the reverse-DNS prefix for Mesh-owned MCP _meta keys.
	MetaPrefix = "com.github.shaullavo.mesh/"
	// MetaUpstreamServerInfo preserves the hosted server's own implementation info.
	MetaUpstreamServerInfo = MetaPrefix + "upstreamServerInfo"

	keyResult = "result"
	keyMeta   = "_meta"
	keyParams = "params"
)

var errNotJSONObject = errors.New("mcp connect: expected a JSON object")

// rewriteResultServerInfo preserves the upstream server's identity in
// com.github.shaullavo.mesh/upstreamServerInfo while advertising Mesh as the
// direct frontend in io.modelcontextprotocol/serverInfo. All other JSON fields
// and unknown vendor extensions are kept byte-exact.
func rewriteResultServerInfo(raw []byte, meshServerInfo, fallbackUpstream json.RawMessage) ([]byte, json.RawMessage, error) {
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
