package mcpwire

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
)

func cacheable(method string) bool {
	switch method {
	case MethodDiscover, MethodListTools, MethodListPrompts, MethodListResources, MethodListResourceTemplates, MethodReadResource:
		return true
	default:
		return false
	}
}

// Complete builds a Mesh reply while preserving method fields and vendor metadata.
func Complete(request Message, result json.RawMessage, serverInfo json.RawMessage, ttlMS int64) ([]byte, error) {
	if len(result) > MaxMessage || len(serverInfo) > MaxMessage {
		return nil, ErrTooLarge
	}
	fields, ok := decodeObject(result)
	if !ok || request.kind != Request || ttlMS < 0 || serverInfo == nil || !optionalImplementation(serverInfo) {
		return nil, fmt.Errorf("%w: invalid arguments for a complete MCP reply", ErrPeerMessage)
	}
	meta, ok := resultMeta(fields)
	if !ok {
		return nil, fmt.Errorf("%w: invalid MCP result metadata", ErrPeerMessage)
	}
	fields["resultType"] = json.RawMessage(`"complete"`)
	if cacheable(request.method) {
		if request.continuation() {
			ttlMS = 0
		}
		fields["ttlMs"] = json.RawMessage(strconv.FormatInt(ttlMS, 10))
		fields["cacheScope"] = json.RawMessage(`"private"`)
	}
	if request.method == MethodDiscover {
		fields["supportedVersions"] = json.RawMessage(`["` + Version + `"]`)
	}
	meta[ServerInfoKey] = serverInfo
	if request.method == MethodListen {
		meta[SubscriptionID] = json.RawMessage(request.id.raw)
	}
	var err error
	fields["_meta"], err = json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("encode MCP result metadata: %w", err)
	}
	raw, err := encodeMessage(map[string]any{"jsonrpc": jsonRPCVersion, "id": request.id, "result": fields})
	if err != nil {
		return nil, err
	}
	reply, fault := Parse(raw)
	if fault != nil {
		return nil, fault
	}
	if err := validateResult(reply, request); err != nil {
		return nil, err
	}
	return raw, nil
}

func resultMeta(fields object) (object, bool) {
	if fields["_meta"] == nil {
		return make(object), true
	}
	return decodeObject(fields["_meta"])
}

func validateResult(reply, request Message) error {
	result, ok := decodeObject(reply.Result())
	if !ok {
		return peerError("expected a result object")
	}
	kind, _ := decodeString(result["resultType"])
	meta, ok := resultMeta(result)
	if !ok || !optionalImplementation(meta[ServerInfoKey]) || (request.method != MethodListen && meta[SubscriptionID] != nil) {
		return peerError("invalid result metadata")
	}
	if kind == "input_required" && validInputRequired(request, result) {
		return nil
	}
	if kind != "complete" || !validComplete(request, result, meta) {
		return peerError("invalid modern result for %s", request.method)
	}
	return nil
}

func validComplete(request Message, result, meta object) bool {
	if result["inputRequests"] != nil || result["requestState"] != nil {
		return false
	}
	if cacheable(request.method) && !validCacheHints(result, request.continuation()) {
		return false
	}
	return validMethodResult(request, result, meta)
}

func validCacheHints(result object, continuation bool) bool {
	scope, _ := decodeString(result["cacheScope"])
	ttl, ok := ParseID(result["ttlMs"])
	return ok && integerID.MatchString(ttl.key) && ttl.key[0] != '-' &&
		(!continuation || ttl.key == "0") && (scope == "private" || scope == "public")
}

func validMethodResult(request Message, result, meta object) bool {
	switch request.method {
	case MethodDiscover:
		versions, ok := stringArray(result["supportedVersions"])
		caps, hasCaps := decodeObject(result["capabilities"])
		return ok && slices.Contains(versions, Version) && hasCaps &&
			validCapabilities(caps, true) && optionalString(result["instructions"])
	case MethodListTools, MethodListPrompts, MethodListResources, MethodListResourceTemplates:
		return validListResult(request.method, result)
	case MethodCallTool:
		return validObjectArray(result["content"], validContent) && optionalBool(result["isError"])
	case MethodGetPrompt:
		return optionalString(result["description"]) && validObjectArray(result["messages"], validPromptMessage)
	case MethodReadResource:
		return validObjectArray(result["contents"], validResourceContent)
	case MethodComplete:
		completion, ok := decodeObject(result["completion"])
		values, valid := stringArray(completion["values"])
		total := completion["total"]
		return ok && valid && len(values) <= 100 && (total == nil || isNumber(total)) && optionalBool(completion["hasMore"])
	case MethodListen:
		id, ok := ParseID(meta[SubscriptionID])
		return ok && request.id.Equal(id)
	default:
		return false
	}
}

func validListResult(method string, result object) bool {
	field, validator := keyTools, validTool
	switch method {
	case MethodListPrompts:
		field, validator = keyPrompts, func(item object) bool { return hasStrings(item, "name") }
	case MethodListResources:
		field, validator = keyResources, func(item object) bool { return hasStrings(item, "name", "uri") }
	case MethodListResourceTemplates:
		field, validator = "resourceTemplates", func(item object) bool { return hasStrings(item, "name", "uriTemplate") }
	}
	return optionalString(result["nextCursor"]) && validObjectArray(result[field], validator)
}

func validTool(tool object) bool {
	schema, ok := decodeObject(tool["inputSchema"])
	kind, _ := decodeString(schema["type"])
	return ok && kind == "object" &&
		hasStrings(tool, "name") && optionalObject(tool["outputSchema"])
}

func validPromptMessage(message object) bool {
	role, _ := decodeString(message["role"])
	content, ok := decodeObject(message["content"])
	return (role == "user" || role == "assistant") && ok && validContent(content)
}

func validContent(content object) bool {
	kind, _ := decodeString(content["type"])
	switch kind {
	case "text":
		return hasStrings(content, "text")
	case "image", "audio":
		return hasStrings(content, "data", "mimeType")
	case "resource_link":
		return hasStrings(content, "uri", "name")
	case "resource":
		resource, ok := decodeObject(content["resource"])
		return ok && validResourceContent(resource)
	default:
		return false
	}
}

func validResourceContent(resource object) bool {
	return hasStrings(resource, "uri") &&
		((hasStrings(resource, "text") && resource["blob"] == nil) ||
			(hasStrings(resource, "blob") && resource["text"] == nil)) &&
		optionalString(resource["mimeType"])
}

func validObjectArray(raw []byte, validate func(object) bool) bool {
	var entries []json.RawMessage
	values, ok := valueAfterDecode(raw, &entries, '[')
	if !ok {
		return false
	}
	for _, value := range values {
		fields, ok := decodeObject(value)
		if !ok || !validate(fields) {
			return false
		}
	}
	return true
}

func stringArray(raw []byte) ([]string, bool) {
	var entries []json.RawMessage
	values, ok := valueAfterDecode(raw, &entries, '[')
	if !ok {
		return nil, false
	}
	items := make([]string, 0, len(values))
	for _, value := range values {
		item, ok := decodeString(value)
		if !ok {
			return nil, false
		}
		items = append(items, item)
	}
	return items, true
}

func validInputRequired(request Message, result object) bool {
	switch request.method {
	case MethodCallTool, MethodGetPrompt, MethodReadResource:
	default:
		return false
	}
	if result["ttlMs"] != nil || result["cacheScope"] != nil || !optionalString(result["requestState"]) {
		return false
	}
	if result["inputRequests"] == nil {
		return result["requestState"] != nil
	}
	inputs, ok := decodeObject(result["inputRequests"])
	if !ok || (len(inputs) == 0 && result["requestState"] == nil) {
		return false
	}
	caps, _ := decodeObject(request.meta[ClientCapsKey])
	for _, input := range inputs {
		if !validInputRequest(input, caps) {
			return false
		}
	}
	return true
}

func validInputRequest(raw []byte, caps object) bool {
	input, _ := decodeObject(raw)
	method, _ := decodeString(input["method"])
	if input["id"] != nil || input["jsonrpc"] != nil {
		return false
	}
	params, _ := decodeObject(input["params"])
	switch method {
	case "elicitation/create":
		return validElicitation(params, caps)
	case "roots/list":
		return isObject(caps["roots"]) && optionalObject(input["params"])
	case "sampling/createMessage":
		return validSampling(params, caps)
	default:
		return false
	}
}

func validElicitation(params, caps object) bool {
	capability, ok := decodeObject(caps["elicitation"])
	if !ok || !hasStrings(params, "message") || !optionalString(params["mode"]) {
		return false
	}
	mode, _ := decodeString(params["mode"])
	switch mode {
	case "", "form":
		schema, _ := decodeObject(params["requestedSchema"])
		kind, _ := decodeString(schema["type"])
		return (isObject(capability["form"]) || len(capability) == 0) && kind == "object"
	case "url":
		return isObject(capability["url"]) && hasStrings(params, "url", "elicitationId")
	default:
		return false
	}
}

func validSampling(params, caps object) bool {
	sampling, ok := decodeObject(caps["sampling"])
	return ok && isNumber(params["maxTokens"]) &&
		validObjectArray(params["messages"], validSamplingMessage) &&
		((params[keyTools] == nil && params["toolChoice"] == nil) || isObject(sampling[keyTools]))
}

func validSamplingMessage(message object) bool {
	role, _ := decodeString(message["role"])
	return (role == "user" || role == "assistant") &&
		(isObject(message["content"]) || validObjectArray(message["content"], func(object) bool { return true }))
}

func validateRPCError(message Message) (int, error) {
	fields, ok := decodeObject(message.fields["error"])
	code, err := strconv.Atoi(string(fields["code"]))
	if !ok || err != nil || !hasStrings(fields, "message") {
		return 0, peerError("invalid JSON-RPC error object")
	}
	if code == -32002 || (code <= -32023 && code >= -32099) {
		return 0, peerError("error code is outside the pinned MCP profile")
	}
	data, _ := decodeObject(fields["data"])
	switch code {
	case UnsupportedVersion:
		supported, ok := stringArray(data["supported"])
		if !ok || len(supported) == 0 || !hasStrings(data, "requested") {
			return 0, peerError("invalid unsupported-version error data")
		}
	case MissingCapability:
		caps, ok := decodeObject(data["requiredCapabilities"])
		if !ok || len(caps) == 0 || !validCapabilities(caps, false) {
			return 0, peerError("invalid required-capability error data")
		}
	}
	return code, nil
}

func peerError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrPeerMessage, fmt.Sprintf(format, args...))
}
