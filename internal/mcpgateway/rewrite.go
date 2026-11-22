package mcpgateway

import (
	"encoding/json"
	"fmt"

	"github.com/shaul/mesh/internal/mcpwire"
)

// uriRewriter turns an upstream resource URI into its gateway form. Only
// resource URI fields change; text and unrelated URLs stay as they are.
type uriRewriter struct {
	hostID, serviceID string
}

func (r uriRewriter) uri(raw json.RawMessage) (json.RawMessage, error) {
	var original string
	if err := json.Unmarshal(raw, &original); err != nil {
		return nil, fmt.Errorf("gateway: resource URI is not a string: %w", err)
	}
	encoded, err := EncodeURI(r.hostID, r.serviceID, original)
	if err != nil {
		return nil, err
	}
	return quoted(encoded), nil
}

func (r uriRewriter) field(raw []byte, path ...string) ([]byte, error) {
	value := member(raw, path...)
	if value == nil {
		return raw, nil
	}
	encoded, err := r.uri(value)
	if err != nil {
		return nil, err
	}
	return setMember(raw, path, encoded)
}

// content rewrites one content block: resource links and embedded resources.
func (r uriRewriter) content(block json.RawMessage) (json.RawMessage, error) {
	kind, _ := stringMember(block, "type")
	switch kind {
	case "resource_link":
		return r.field(block, keyURI)
	case "resource":
		return r.field(block, "resource", keyURI)
	default:
		return block, nil
	}
}

func (r uriRewriter) contentList(raw []byte, path ...string) ([]byte, error) {
	return mapArray(raw, path, r.content)
}

// promptMessage rewrites a prompt message whose content is one block or a
// list of blocks.
func (r uriRewriter) promptMessage(message json.RawMessage) (json.RawMessage, error) {
	content := member(message, "content")
	if content == nil {
		return message, nil
	}
	if _, err := decodeArray(content); err == nil {
		return r.contentList(message, "content")
	}
	rewritten, err := r.content(content)
	if err != nil {
		return nil, err
	}
	return setMember(message, []string{"content"}, rewritten)
}

// response rewrites the resource fields of a forwarded terminal response.
func (r uriRewriter) response(method string, raw []byte) ([]byte, error) {
	if member(raw, "result") == nil {
		return raw, nil
	}
	switch method {
	case mcpwire.MethodCallTool:
		return r.contentList(raw, "result", "content")
	case mcpwire.MethodGetPrompt:
		return mapArray(raw, []string{"result", "messages"}, r.promptMessage)
	case mcpwire.MethodReadResource:
		return mapArray(raw, []string{"result", "contents"}, func(item json.RawMessage) (json.RawMessage, error) {
			return r.field(item, keyURI)
		})
	default:
		return raw, nil
	}
}

// notification rewrites a resource update's URI.
func (r uriRewriter) notification(raw []byte) ([]byte, error) {
	method, _ := stringMember(raw, "method")
	if method != "notifications/resources/updated" {
		return raw, nil
	}
	return r.field(raw, keyParams, keyURI)
}
