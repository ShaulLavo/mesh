package mcphost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shaul/mesh/internal/mcpwire"
)

var errNotObject = errors.New("mcp host: expected a JSON object")

// setMember replaces one member at path and keeps every other value's bytes.
// A nil value removes the member. Missing intermediate objects are created
// only when a value is being set.
func setMember(raw []byte, path []string, value json.RawMessage) ([]byte, error) {
	fields, err := decodeMembers(raw)
	if err != nil {
		return nil, err
	}
	if len(path) == 1 {
		if value == nil {
			delete(fields, path[0])
		} else {
			fields[path[0]] = value
		}
		return encodeJSON(fields)
	}
	child := fields[path[0]]
	if child == nil && value == nil {
		return encodeJSON(fields)
	}
	if child == nil {
		child = json.RawMessage("{}")
	}
	updated, err := setMember(child, path[1:], value)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path[0], err)
	}
	fields[path[0]] = updated
	return encodeJSON(fields)
}

// member reads one value at path, or nil when any step is absent.
func member(raw []byte, path ...string) json.RawMessage {
	for _, name := range path {
		fields, err := decodeMembers(raw)
		if err != nil {
			return nil
		}
		raw = fields[name]
	}
	return raw
}

func decodeMembers(raw []byte) (map[string]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, errNotObject
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("mcp host: decode object: %w", err)
	}
	return fields, nil
}

func encodeJSON(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("mcp host: encode JSON: %w", err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

func jsonString(value string) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func decodeJSONString(raw []byte) (string, bool) {
	var value string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

// reparse validates rewritten bytes against the wire limits again, so a
// rewrite that grows a message past the 3.5 MiB cap is caught here.
func reparse(raw []byte) (mcpwire.Message, error) {
	m, fault := mcpwire.Parse(raw)
	if fault != nil {
		return mcpwire.Message{}, fmt.Errorf("mcp host: rewritten message: %w", fault)
	}
	return m, nil
}
