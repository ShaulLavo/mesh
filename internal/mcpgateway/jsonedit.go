package mcpgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

type object = map[string]json.RawMessage

// MCP member names the gateway reads or rewrites.
const (
	keyParams      = "params"
	keyMeta        = "_meta"
	keyName        = "name"
	keyURI         = "uri"
	keyTools       = "tools"
	keyPrompts     = "prompts"
	keyResources   = "resources"
	keyListChanged = "listChanged"
	jsonTrue       = "true"
	flagTools      = "toolsListChanged"
	flagPrompts    = "promptsListChanged"
	flagResources  = "resourcesListChanged"
)

var errNotObject = errors.New("gateway: expected a JSON object")

func decodeObject(raw []byte) (object, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errNotObject
	}
	var fields object
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, fmt.Errorf("gateway: decode JSON object: %w", err)
	}
	return fields, nil
}

func decodeArray(raw []byte) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errors.New("gateway: expected a JSON array")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("gateway: decode JSON array: %w", err)
	}
	return items, nil
}

// encode writes compact JSON without HTML escaping, so text that passes
// through keeps its exact characters.
func encode(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("gateway: encode JSON: %w", err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// member returns a nested member, or nil when any step is missing.
func member(raw []byte, path ...string) json.RawMessage {
	current := json.RawMessage(raw)
	for _, name := range path {
		fields, err := decodeObject(current)
		if err != nil {
			return nil
		}
		next, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(next), []byte("null")) {
			return nil
		}
		current = next
	}
	return current
}

func stringMember(raw []byte, path ...string) (string, bool) {
	value := member(raw, path...)
	if value == nil {
		return "", false
	}
	var out string
	if json.Unmarshal(value, &out) != nil {
		return "", false
	}
	return out, true
}

// setMember replaces a nested member, creating objects on the way. A nil
// value deletes it.
func setMember(raw []byte, path []string, value json.RawMessage) ([]byte, error) {
	if len(path) == 0 {
		return nil, errors.New("gateway: empty member path")
	}
	fields, err := decodeObject(raw)
	if err != nil {
		if len(bytes.TrimSpace(raw)) != 0 {
			return nil, err
		}
		fields = object{}
	}
	if len(path) == 1 {
		if value == nil {
			delete(fields, path[0])
		} else {
			fields[path[0]] = value
		}
		return encode(fields)
	}
	child, err := setMember(fields[path[0]], path[1:], value)
	if err != nil {
		return nil, err
	}
	fields[path[0]] = child
	return encode(fields)
}

func quoted(value string) json.RawMessage {
	raw, _ := encode(value)
	return raw
}

// mapArray rewrites each element of the array at path, if present.
func mapArray(raw []byte, path []string, rewrite func(json.RawMessage) (json.RawMessage, error)) ([]byte, error) {
	array := member(raw, path...)
	if array == nil {
		return raw, nil
	}
	items, err := decodeArray(array)
	if err != nil {
		return nil, err
	}
	for i, item := range items {
		if items[i], err = rewrite(item); err != nil {
			return nil, err
		}
	}
	encoded, err := encode(items)
	if err != nil {
		return nil, err
	}
	return setMember(raw, path, encoded)
}
