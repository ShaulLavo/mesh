package mcpwire

import (
	"net/url"
	"slices"
)

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
	if !ok || !hasStrings(params, "message") {
		return false
	}
	mode, _ := decodeString(params["mode"])
	switch {
	case mode == "form" || params["mode"] == nil:
		return (isObject(capability["form"]) || implicitFormElicitation(capability)) && validElicitationForm(params["requestedSchema"])
	case mode == "url":
		address, _ := decodeString(params["url"])
		target, err := url.Parse(address)
		return isObject(capability["url"]) && err == nil && target.IsAbs()
	default:
		return false
	}
}

func validElicitationForm(raw []byte) bool {
	schema, _ := decodeObject(raw)
	kind, _ := decodeString(schema["type"])
	properties, ok := decodeObject(schema["properties"])
	if kind != "object" || !ok || !optionalString(schema["$schema"]) || !optionalStringArray(schema["required"]) {
		return false
	}
	for _, property := range properties {
		if !validElicitationProperty(property) {
			return false
		}
	}
	return true
}

func validElicitationProperty(raw []byte) bool {
	field, _ := decodeObject(raw)
	kind, _ := decodeString(field["type"])
	if !optionalString(field["title"]) || !optionalString(field["description"]) {
		return false
	}
	switch kind {
	case schemaString:
		return validElicitationString(field)
	case "number", schemaInteger:
		return optionalNumber(field["minimum"]) && optionalNumber(field["maximum"]) && optionalNumber(field["default"])
	case schemaBoolean:
		return optionalBool(field["default"])
	case "array":
		return validElicitationEnumArray(field)
	default:
		return false
	}
}

func validElicitationString(field object) bool {
	if !optionalString(field["default"]) || !optionalStringArray(field["enum"]) || !optionalStringArray(field["enumNames"]) {
		return false
	}
	if field["oneOf"] != nil && !validObjectArray(field["oneOf"], validEnumOption) {
		return false
	}
	return optionalNumber(field["minLength"]) && optionalNumber(field["maxLength"]) &&
		optionalEnum(field["format"], "email", "uri", "date", "date-time")
}

func validElicitationEnumArray(field object) bool {
	items, _ := decodeObject(field["items"])
	kind, _ := decodeString(items["type"])
	_, enum := stringArray(items["enum"])
	options := validObjectArray(items["anyOf"], validEnumOption)
	return ((kind == schemaString && enum) || options) &&
		optionalNumber(field["minItems"]) && optionalNumber(field["maxItems"]) && optionalStringArray(field["default"])
}

func validEnumOption(option object) bool { return hasStrings(option, "const", "title") }

func validSampling(params, caps object) bool {
	sampling, ok := decodeObject(caps["sampling"])
	if !ok || !isNumber(params["maxTokens"]) || !validObjectArray(params["messages"], validSamplingMessage) {
		return false
	}
	return optionalString(params["systemPrompt"]) && optionalNumber(params["temperature"]) &&
		optionalStringArray(params["stopSequences"]) && optionalObject(params["metadata"]) &&
		optionalEnum(params["includeContext"], "none", "thisServer", "allServers") &&
		validModelPreferences(params["modelPreferences"]) && validSamplingTools(params, sampling)
}

func validSamplingTools(params, sampling object) bool {
	if params[keyTools] == nil && params["toolChoice"] == nil {
		return true
	}
	if !isObject(sampling[keyTools]) {
		return false
	}
	if params[keyTools] != nil && !validObjectArray(params[keyTools], validTool) {
		return false
	}
	choice, ok := decodeObject(params["toolChoice"])
	return params["toolChoice"] == nil || (ok && optionalEnum(choice["mode"], "auto", "required", "none"))
}

func validModelPreferences(raw []byte) bool {
	if raw == nil {
		return true
	}
	fields, ok := decodeObject(raw)
	if !ok || !optionalNumber(fields["costPriority"]) || !optionalNumber(fields["speedPriority"]) ||
		!optionalNumber(fields["intelligencePriority"]) {
		return false
	}
	return fields["hints"] == nil || validObjectArray(fields["hints"], func(hint object) bool {
		return optionalString(hint["name"])
	})
}

func validSamplingMessage(message object) bool {
	role, _ := decodeString(message["role"])
	if (role != "user" && role != "assistant") || !optionalObject(message["_meta"]) {
		return false
	}
	raw := message["content"]
	if content, ok := decodeObject(raw); ok {
		return validSamplingBlock(content)
	}
	return validObjectArray(raw, validSamplingBlock)
}

func validSamplingBlock(content object) bool {
	kind, _ := decodeString(content["type"])
	switch kind {
	case "text", "image", "audio":
		return validContent(content)
	case "tool_use":
		return hasStrings(content, "id", "name") && isObject(content["input"]) && optionalObject(content["_meta"])
	case "tool_result":
		return hasStrings(content, "toolUseId") && validObjectArray(content["content"], validContent) &&
			optionalBool(content["isError"]) && optionalObject(content["_meta"])
	default:
		return false
	}
}

func optionalNumber(raw []byte) bool { return raw == nil || isNumber(raw) }

func optionalStringArray(raw []byte) bool {
	if raw == nil {
		return true
	}
	_, ok := stringArray(raw)
	return ok
}

func optionalEnum(raw []byte, values ...string) bool {
	value, ok := decodeString(raw)
	return raw == nil || (ok && slices.Contains(values, value))
}
