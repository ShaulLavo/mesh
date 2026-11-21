// Package mcpwire validates stateless MCP messages without interpreting vendor payloads.
package mcpwire

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"regexp"
	"sync"
)

const (
	MethodDiscover              = "server/discover"
	MethodListTools             = "tools/list"
	MethodCallTool              = "tools/call"
	MethodListPrompts           = "prompts/list"
	MethodGetPrompt             = "prompts/get"
	MethodListResources         = "resources/list"
	MethodListResourceTemplates = "resources/templates/list"
	MethodReadResource          = "resources/read"
	MethodComplete              = "completion/complete"
	MethodListen                = "subscriptions/listen"
	jsonRPCVersion              = "2.0"
	keyExperimental             = "experimental"
	keyExtensions               = "extensions"
	keyTools                    = "tools"
	keyPrompts                  = "prompts"
	keyResources                = "resources"

	Version        = "2026-07-28"
	MaxMessage     = 7 << 19
	MaxStdioDrain  = 16 << 20
	VersionKey     = "io.modelcontextprotocol/protocolVersion"
	ClientCapsKey  = "io.modelcontextprotocol/clientCapabilities"
	ClientInfoKey  = "io.modelcontextprotocol/clientInfo"
	ServerInfoKey  = "io.modelcontextprotocol/serverInfo"
	SubscriptionID = "io.modelcontextprotocol/subscriptionId"
	LogLevelKey    = "io.modelcontextprotocol/logLevel"

	ParseError         = -32700
	InvalidRequest     = -32600
	MethodNotFound     = -32601
	InvalidParams      = -32602
	HeaderMismatch     = -32020
	MissingCapability  = -32021
	UnsupportedVersion = -32022
)

var (
	ErrTooLarge    = errors.New("MCP message exceeds the byte limit")
	ErrFramingLost = errors.New("MCP message boundary lost")
	ErrInterrupted = errors.New("MCP stream ended before a terminal response")
	ErrPeerMessage = errors.New("invalid MCP peer message")
	integerID      = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
)

type object = map[string]json.RawMessage

// ID preserves the wire representation. Its key compares decoded strings and
// exact integers without converting through float64.
type ID struct {
	key string
	raw string
}

func (id ID) Valid() bool { return id.raw != "" }

func (id ID) Equal(other ID) bool { return id.Valid() && id.key == other.key }

func (id ID) MarshalJSON() ([]byte, error) {
	if id.raw == "" {
		return []byte("null"), nil
	}
	return []byte(id.raw), nil
}

func ParseID(raw json.RawMessage) (ID, bool) {
	if len(raw) > 0 && raw[0] == '"' {
		if !strictJSON(raw) {
			return ID{}, false
		}
		value, ok := decodeString(raw)
		return ID{key: "s" + value, raw: string(raw)}, ok
	}
	if !integerID.Match(raw) {
		return ID{}, false
	}
	key := string(raw)
	if key == "-0" {
		key = "0"
	}
	return ID{key: key, raw: string(raw)}, true
}

type Kind uint8

const (
	Request Kind = iota + 1
	Notification
	Response
)

type Message struct {
	kind   Kind
	raw    json.RawMessage
	fields object
	params object
	meta   object
	id     ID
	method string
}

func (m Message) Kind() Kind                   { return m.kind }
func (m Message) ID() ID                       { return m.id }
func (m Message) Method() string               { return m.method }
func (m Message) Raw() json.RawMessage         { return m.raw }
func (m Message) Result() json.RawMessage      { return m.fields["result"] }
func (m Message) ErrorObject() json.RawMessage { return m.fields["error"] }
func (m Message) Params() json.RawMessage      { return m.fields["params"] }

type Fault struct {
	ID      ID     `json:"-"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (f *Fault) Error() string { return f.Message }

// Reply includes a null ID when no usable request ID was present.
func (f *Fault) Reply() ([]byte, error) {
	return encodeMessage(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      ID     `json:"id"`
		Error   *Fault `json:"error"`
	}{jsonRPCVersion, f.ID, f})
}

// Parse borrows raw for the lifetime of the message. Unknown fields are kept.
func Parse(raw []byte) (Message, *Fault) {
	if len(raw) > MaxMessage {
		return Message{}, &Fault{Code: InvalidRequest, Message: ErrTooLarge.Error()}
	}
	fields, ok := decodeObject(raw)
	if !ok {
		return Message{}, invalidEnvelope(raw)
	}
	id, usable := ParseID(fields["id"])
	m := Message{raw: raw, id: id, fields: fields}
	if version, _ := decodeString(fields["jsonrpc"]); version != jsonRPCVersion {
		return m, m.fault(InvalidRequest, "Expected JSON-RPC 2.0")
	}
	if _, hasMethod := fields["method"]; hasMethod {
		return parseCall(m, usable)
	}
	return parseReply(m, usable)
}

func invalidEnvelope(raw []byte) *Fault {
	if !json.Valid(raw) {
		return &Fault{Code: ParseError, Message: "Malformed JSON"}
	}
	if !strictJSON(raw) {
		return &Fault{Code: InvalidRequest, Message: "JSON must use UTF-8 and unique object members"}
	}
	return &Fault{Code: InvalidRequest, Message: "Expected one JSON-RPC object"}
}

func parseCall(m Message, usable bool) (Message, *Fault) {
	m.kind = Notification
	if m.fields["id"] != nil {
		m.kind = Request
	}
	m.method, _ = decodeString(m.fields["method"])
	if m.method == "" || m.fields["result"] != nil || m.fields["error"] != nil || (m.kind == Request && !usable) {
		return m, m.fault(InvalidRequest, "Invalid JSON-RPC request or notification")
	}
	m.params, _ = decodeObject(m.fields["params"])
	m.meta, _ = decodeObject(m.params["_meta"])
	return m, nil
}

func parseReply(m Message, usable bool) (Message, *Fault) {
	result, rpcError := m.fields["result"], m.fields["error"]
	if result != nil || rpcError != nil {
		m.kind = Response
	}
	if (result == nil) == (rpcError == nil) || m.fields["params"] != nil {
		return m, m.fault(InvalidRequest, "Expected exactly one JSON-RPC result or error")
	}
	if result != nil && !usable {
		return m, m.fault(InvalidRequest, "Invalid JSON-RPC response ID")
	}
	if rpcError != nil && !usable && m.fields["id"] != nil && !bytes.Equal(m.fields["id"], []byte("null")) {
		return m, m.fault(InvalidRequest, "Invalid JSON-RPC error ID")
	}
	return m, nil
}

func (m Message) fault(code int, message string) *Fault {
	return &Fault{ID: m.id, Code: code, Message: message}
}

// Object fields share the owning frame's bytes, including when several layers
// inspect the same large tool call or result.
func decodeObject(raw []byte) (object, bool) {
	raw = bytes.Trim(raw, " \t\r\n")
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	decoder := jsontext.NewDecoder(bytes.NewBuffer(raw))
	if _, err := decoder.ReadToken(); err != nil {
		return nil, false
	}
	fields := make(object)
	for decoder.PeekKind() == '"' {
		name, value, ok := readObjectField(decoder, raw)
		if !ok {
			return nil, false
		}
		fields[name] = value
	}
	end, err := decoder.ReadToken()
	return fields, err == nil && end.Kind() == '}' && decoder.InputOffset() == int64(len(raw))
}

func readObjectField(decoder *jsontext.Decoder, raw []byte) (string, json.RawMessage, bool) {
	key, err := decoder.ReadToken()
	if err != nil {
		return "", nil, false
	}
	name := key.String()
	value, err := decoder.ReadValue()
	if err != nil {
		return "", nil, false
	}
	end := decoder.InputOffset()
	return name, raw[end-int64(len(value)) : end], true
}

func decodeString(raw []byte) (string, bool) {
	var value string
	return valueAfterDecode(raw, &value, '"')
}

func valueAfterDecode[T any](raw []byte, value *T, prefix byte) (T, bool) {
	raw = bytes.TrimSpace(raw)
	ok := len(raw) > 0 && raw[0] == prefix && json.Unmarshal(raw, value) == nil
	return *value, ok
}

func strictJSON(raw []byte) bool { return jsontext.Value(raw).IsValid() }

func isObject(raw []byte) bool { _, ok := decodeObject(raw); return ok }

// WithID changes one envelope field and preserves all other JSON values.
func (m Message) WithID(id ID) ([]byte, error) {
	if m.kind == Notification || id.raw == "" {
		return nil, fmt.Errorf("%w: ID replacement requires a request or response and a valid ID", ErrPeerMessage)
	}
	fields := copyObject(m.fields)
	fields["id"] = json.RawMessage(id.raw)
	return encodeMessage(fields)
}

func copyObject(fields object) object {
	out := make(object, len(fields))
	for key, value := range fields {
		out[key] = value
	}
	return out
}

func encodeMessage(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("encode MCP message: %w", err)
	}
	if output.Len()-1 > MaxMessage {
		return nil, ErrTooLarge
	}
	return output.Bytes()[:output.Len()-1], nil
}

// ActiveIDs arbitrates simultaneous requests on one multiplexed channel.
type ActiveIDs struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

func (a *ActiveIDs) Begin(m Message) *Fault {
	if m.kind != Request || m.id.raw == "" {
		return m.fault(InvalidRequest, "Expected a request ID")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, active := a.ids[m.id.key]; active {
		// Null keeps the rejection from being mistaken for the first request's result.
		return &Fault{Code: InvalidRequest, Message: "Request ID is already active"}
	}
	if a.ids == nil {
		a.ids = make(map[string]struct{})
	}
	a.ids[m.id.key] = struct{}{}
	return nil
}

func (a *ActiveIDs) End(id ID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.ids, id.key)
}

// WithMetadata replaces or removes only the named metadata fields. A nil
// value removes that field. Route policy chooses which fields to change.
func (m Message) WithMetadata(replacements map[string]json.RawMessage) ([]byte, error) {
	location := "params"
	if m.kind == Response {
		location = "result"
	}
	fields, ok := decodeObject(m.fields[location])
	if !ok {
		return nil, peerError("message has no %s object for metadata replacement", location)
	}
	meta, ok := resultMeta(fields)
	if !ok {
		return nil, peerError("invalid metadata")
	}
	for key, value := range replacements {
		if value == nil {
			delete(meta, key)
			continue
		}
		if !strictJSON(value) {
			return nil, peerError("invalid metadata replacement")
		}
		meta[key] = value
	}
	return m.encodeMetadata(location, fields, meta)
}

func (m Message) encodeMetadata(location string, fields, meta object) ([]byte, error) {
	raw, err := encodeMessage(meta)
	if err != nil {
		return nil, err
	}
	fields["_meta"] = raw
	payload, err := encodeMessage(fields)
	if err != nil {
		return nil, err
	}
	envelope := copyObject(m.fields)
	envelope[location] = payload
	return encodeMessage(envelope)
}
