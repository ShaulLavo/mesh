package mcpwire

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
)

// Methods maps implemented core methods to their required client capabilities.
// An empty entry means the method requires no optional client capability.
type Methods map[string]map[string]any

type ClientInput struct {
	Message      Message
	Cancellation ID
	// Ignored notifications expose only the method for optional diagnostics.
	Ignored bool
	Fault   *Fault
}

func StdioInput(raw []byte, methods Methods) ClientInput {
	m, fault := Parse(raw)
	input := ClientInput{Message: m, Fault: fault}
	if m.kind == Notification {
		id, valid := cancellationID(m)
		input.Cancellation = id
		input.Ignored = fault != nil || !valid
		input.Fault = nil
		return input
	}
	if m.kind == Response {
		input.Fault = nil
		input.Ignored = true
		return input
	}
	if fault == nil {
		input.Fault = ValidateRequest(m, methods)
	}
	return input
}

// ValidateRequest applies to H1, H3 and H4. HTTP checks its headers first.
func ValidateRequest(m Message, methods Methods) *Fault {
	if m.kind != Request {
		return m.fault(InvalidRequest, "Expected an MCP request")
	}
	if m.method == "initialize" {
		return m.fault(MethodNotFound, "Mesh supports MCP "+Version)
	}
	return validateRequest(m, methods)
}

func validateRequest(m Message, methods Methods) *Fault {
	version, versionOK := decodeString(m.meta[VersionKey])
	if !versionOK {
		return m.fault(InvalidParams, "Missing or invalid MCP protocol version metadata")
	}
	if version != Version {
		fault := m.fault(UnsupportedVersion, "Unsupported MCP protocol version")
		fault.Data = map[string]any{"requested": version, "supported": []string{Version}}
		return fault
	}
	caps, ok := decodeObject(m.meta[ClientCapsKey])
	if !ok || !validCapabilities(caps, false) || !optionalImplementation(m.meta[ClientInfoKey]) {
		return m.fault(InvalidParams, "Missing or invalid MCP client metadata")
	}
	required, implemented := methods[m.method]
	if !implemented || !coreMethod(m.method) {
		return m.fault(MethodNotFound, "MCP method not found")
	}
	if missing := missingCapabilities(caps, required); len(missing) != 0 {
		fault := m.fault(MissingCapability, "Required MCP client capability is missing")
		fault.Data = map[string]any{"requiredCapabilities": missing}
		return fault
	}
	if !validParams(m) {
		return m.fault(InvalidParams, "Invalid MCP method parameters")
	}
	return nil
}

func coreMethod(method string) bool {
	switch method {
	case MethodDiscover, MethodListTools, MethodCallTool, MethodListPrompts, MethodGetPrompt, MethodListResources,
		MethodListResourceTemplates, MethodReadResource, MethodComplete, MethodListen:
		return true
	default:
		return false
	}
}

func validCapabilities(caps object, server bool) bool {
	keys := []string{"roots", "sampling", "elicitation", keyExperimental, keyExtensions}
	if server {
		keys = []string{keyTools, keyPrompts, keyResources, "completions", "logging", keyExperimental, keyExtensions}
	}
	for _, key := range keys {
		if caps[key] == nil {
			continue
		}
		if !validCapability(key, caps[key]) {
			return false
		}
	}
	return true
}

func validCapability(key string, raw []byte) bool {
	fields, ok := decodeObject(raw)
	if !ok {
		return false
	}
	switch key {
	case keyExperimental, keyExtensions:
		return validCapabilityEntries(fields)
	case keyTools, keyPrompts, "roots":
		return optionalBool(fields["listChanged"])
	case keyResources:
		return optionalBool(fields["listChanged"]) && optionalBool(fields["subscribe"])
	case "sampling":
		return optionalObject(fields["context"]) && optionalObject(fields[keyTools])
	case "elicitation":
		return optionalObject(fields["form"]) && optionalObject(fields["url"])
	}
	return true
}

func validCapabilityEntries(fields object) bool {
	for _, extension := range fields {
		if !isObject(extension) {
			return false
		}
	}
	return true
}

func missingCapabilities(caps object, required map[string]any) map[string]any {
	var missing map[string]any
	for name, value := range required {
		absent, ok := missingCapability(caps[name], value)
		if !ok {
			continue
		}
		if missing == nil {
			missing = make(map[string]any)
		}
		missing[name] = absent
	}
	return missing
}

func missingCapability(raw []byte, required any) (any, bool) {
	switch expected := required.(type) {
	case map[string]any:
		declared, ok := decodeObject(raw)
		if !ok {
			return expected, true
		}
		missing := missingCapabilities(declared, expected)
		return missing, len(missing) != 0
	case bool:
		return expected, !bytes.Equal(raw, []byte(strconv.FormatBool(expected)))
	case string:
		declared, ok := decodeString(raw)
		return expected, !ok || declared != expected
	default:
		return required, !isObject(raw)
	}
}

func validParams(m Message) bool {
	if !validContinuation(m) {
		return false
	}
	if raw := m.meta["progressToken"]; raw != nil && !validToken(raw) {
		return false
	}
	if raw := m.meta[LogLevelKey]; raw != nil && !validLogLevel(raw) {
		return false
	}
	switch m.method {
	case MethodCallTool, MethodGetPrompt, MethodReadResource:
		return validNamedParams(m)
	case MethodListTools, MethodListPrompts, MethodListResources, MethodListResourceTemplates:
		return optionalString(m.params["cursor"])
	case MethodListen:
		_, ok := parseFilter(m.params["notifications"])
		return ok
	case MethodComplete:
		return validCompletionParams(m.params)
	default:
		return true
	}
}

func validNamedParams(m Message) bool {
	name, ok := standardName(m)
	raw := m.params["arguments"]
	if !ok || name == "" || !optionalObject(raw) {
		return false
	}
	if m.method != MethodGetPrompt || raw == nil {
		return true
	}
	args, _ := decodeObject(raw)
	for _, value := range args {
		if _, ok := decodeString(value); !ok {
			return false
		}
	}
	return true
}

func validContinuation(m Message) bool {
	if !m.continuation() {
		return true
	}
	switch m.method {
	case MethodCallTool, MethodReadResource, MethodGetPrompt:
		return optionalString(m.params["requestState"]) && validInputResponses(m.params["inputResponses"])
	default:
		return false
	}
}

func validInputResponses(raw []byte) bool {
	if raw == nil {
		return true
	}
	fields, ok := decodeObject(raw)
	if !ok {
		return false
	}
	for _, value := range fields {
		if !validInputResponse(value) {
			return false
		}
	}
	return true
}

func validInputResponse(raw []byte) bool {
	value, ok := decodeObject(raw)
	if !ok {
		return false
	}
	switch {
	case value["action"] != nil:
		action, _ := decodeString(value["action"])
		return (action == "accept" && optionalObject(value["content"])) ||
			((action == "decline" || action == "cancel") && value["content"] == nil)
	case value["roots"] != nil:
		return validObjectArray(value["roots"], func(root object) bool {
			return hasStrings(root, "uri") && optionalString(root["name"])
		})
	default:
		return hasStrings(value, "model") && optionalString(value["stopReason"]) && validSamplingMessage(value)
	}
}

func (m Message) continuation() bool {
	return m.params["inputResponses"] != nil || m.params["requestState"] != nil
}

func validCompletionParams(params object) bool {
	ref, _ := decodeObject(params["ref"])
	kind, _ := decodeString(ref["type"])
	arg, ok := decodeObject(params["argument"])
	if !ok || !hasStrings(arg, "name", "value") || !optionalObject(params["context"]) {
		return false
	}
	switch kind {
	case "ref/prompt":
		return hasStrings(ref, "name")
	case "ref/resource":
		return hasStrings(ref, "uri")
	default:
		return false
	}
}

func cancellationID(m Message) (ID, bool) {
	if m.kind != Notification || m.method != "notifications/cancelled" || !optionalString(m.params["reason"]) ||
		!optionalObject(m.params["_meta"]) {
		return ID{}, false
	}
	return ParseID(m.params["requestId"])
}

func optionalImplementation(raw []byte) bool {
	if raw == nil {
		return true
	}
	value, ok := decodeObject(raw)
	return ok && hasStrings(value, "name", "version")
}

func hasStrings(fields object, keys ...string) bool {
	for _, key := range keys {
		if _, ok := decodeString(fields[key]); !ok {
			return false
		}
	}
	return true
}

func optionalString(raw []byte) bool {
	_, ok := decodeString(raw)
	return raw == nil || ok
}

func optionalObject(raw []byte) bool { return raw == nil || isObject(raw) }

func optionalBool(raw []byte) bool {
	return raw == nil || bytes.Equal(raw, []byte("true")) || bytes.Equal(raw, []byte("false"))
}

func validToken(raw []byte) bool {
	if _, ok := decodeString(raw); ok {
		return true
	}
	return isNumber(raw)
}

func isNumber(raw []byte) bool {
	return len(raw) > 0 && (raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9')) && json.Valid(raw)
}

func validLogLevel(raw []byte) bool {
	level, _ := decodeString(raw)
	return slices.Contains([]string{"debug", "info", "notice", "warning", "error", "critical", "alert", "emergency"}, level)
}

func standardName(m Message) (string, bool) {
	switch m.method {
	case MethodCallTool, MethodGetPrompt:
		return decodeString(m.params["name"])
	case MethodReadResource:
		return decodeString(m.params["uri"])
	default:
		return "", false
	}
}

func requiresName(method string) bool {
	return method == MethodCallTool || method == MethodGetPrompt || method == MethodReadResource
}
