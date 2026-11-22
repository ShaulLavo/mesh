package mcpwire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	ProtocolHeader = "Mcp-Protocol-Version"
	MethodHeader   = "Mcp-Method"
	NameHeader     = "Mcp-Name"
	paramPrefix    = "Mcp-Param-"
	base64Prefix   = "=?base64?"
	base64Suffix   = "?="
)

func EncodeHeaderValue(value string) string {
	if plainHeaderValue(value) && !encodedHeaderValue(value) {
		return value
	}
	return base64Prefix + base64.StdEncoding.EncodeToString([]byte(value)) + base64Suffix
}

func DecodeHeaderValue(value string) (string, error) {
	if !plainHeaderValue(value) {
		return "", fmt.Errorf("invalid MCP header value")
	}
	if !encodedHeaderValue(value) {
		return value, nil
	}
	if len(value) < len(base64Prefix)+len(base64Suffix) {
		return "", fmt.Errorf("invalid Base64 MCP header sentinel")
	}
	encoded := value[len(base64Prefix) : len(value)-len(base64Suffix)]
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || !utf8.Valid(decoded) {
		return "", fmt.Errorf("invalid Base64 UTF-8 MCP header value")
	}
	return string(decoded), nil
}

func encodedHeaderValue(value string) bool {
	return strings.HasPrefix(value, base64Prefix) && strings.HasSuffix(value, base64Suffix)
}

func plainHeaderValue(value string) bool {
	if strings.Trim(value, " \t") != value {
		return false
	}
	for i := range len(value) {
		if c := value[i]; c != '\t' && (c < 0x20 || c > 0x7e) {
			return false
		}
	}
	return true
}

func fieldToken(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			continue
		}
		return false
	}
	return true
}

func headerValues(header http.Header, name string) []string {
	var values []string
	for key, list := range header {
		if strings.EqualFold(key, name) {
			values = append(values, list...)
		}
	}
	return values
}

func headerMatches(headers http.Header, name, want string, encoded bool) bool {
	values := headerValues(headers, name)
	if len(values) != 1 {
		return false
	}
	if encoded {
		value, err := DecodeHeaderValue(values[0])
		return err == nil && value == want
	}
	return plainHeaderValue(values[0]) && values[0] == want
}

// ParameterHeaders is compiled from the selected tool's inputSchema.
type ParameterHeaders struct {
	params []parameterHeader
}

type parameterHeader struct {
	name string
	kind string
	path []string
}

func CompileParameterHeaders(schema json.RawMessage) (ParameterHeaders, error) {
	var result ParameterHeaders
	if len(schema) > MaxMessage || !strictJSON(schema) {
		return result, fmt.Errorf("invalid MCP input schema")
	}
	decoder := json.NewDecoder(bytes.NewReader(schema))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return result, fmt.Errorf("MCP input schema must be an object")
	}
	err := result.scanSchema(fields, nil, true, make(map[string]bool))
	return result, err
}

func (h *ParameterHeaders) scanSchema(node any, path []string, reachable bool, seen map[string]bool) error {
	fields, _ := node.(map[string]any)
	if fields == nil {
		return nil
	}
	return h.scanSchemaObject(fields, path, reachable, seen)
}

func (h *ParameterHeaders) scanSchemaObject(fields map[string]any, path []string, reachable bool, seen map[string]bool) error {
	if _, found := fields["x-mcp-header"]; found {
		if err := h.add(fields, path, reachable, seen); err != nil {
			return err
		}
	}
	for name, node := range fields {
		if err := h.scanSchemaChild(name, node, path, reachable, seen); err != nil {
			return err
		}
	}
	return nil
}

func (h *ParameterHeaders) scanSchemaChild(name string, node any, path []string, reachable bool, seen map[string]bool) error {
	switch name {
	case "properties":
		return h.scanSchemaMap(node, path, reachable, seen)
	case "$defs", "definitions", "patternProperties", "dependentSchemas", "dependencies":
		return h.scanSchemaMap(node, path, false, seen)
	case "allOf", "anyOf", "oneOf", "prefixItems":
		return h.scanSchemaArray(node, path, seen)
	case "items":
		if _, array := node.([]any); array {
			return h.scanSchemaArray(node, path, seen)
		}
		return h.scanSchema(node, path, false, seen)
	case "not", "if", "then", "else", "contains", "propertyNames", "additionalProperties", "additionalItems",
		"unevaluatedProperties", "unevaluatedItems", "contentSchema":
		return h.scanSchema(node, path, false, seen)
	default:
		return nil
	}
}

func (h *ParameterHeaders) scanSchemaMap(node any, path []string, reachable bool, seen map[string]bool) error {
	children, _ := node.(map[string]any)
	for key, child := range children {
		childPath := path
		if reachable {
			childPath = append(childPath, key)
		}
		if err := h.scanSchema(child, childPath, reachable, seen); err != nil {
			return err
		}
	}
	return nil
}

func (h *ParameterHeaders) scanSchemaArray(node any, path []string, seen map[string]bool) error {
	children, _ := node.([]any)
	for _, child := range children {
		if err := h.scanSchema(child, path, false, seen); err != nil {
			return err
		}
	}
	return nil
}

func (h *ParameterHeaders) add(fields map[string]any, path []string, reachable bool, seen map[string]bool) error {
	name, _ := fields["x-mcp-header"].(string)
	kind, _ := fields["type"].(string)
	lower := strings.ToLower(name)
	if !reachable || len(path) == 0 || !fieldToken(name) || seen[lower] ||
		(kind != schemaString && kind != schemaBoolean && kind != schemaInteger) {
		return fmt.Errorf("invalid x-mcp-header at %q", strings.Join(path, "."))
	}
	seen[lower] = true
	h.params = append(h.params, parameterHeader{name: paramPrefix + name, kind: kind, path: append([]string(nil), path...)})
	return nil
}

func (h ParameterHeaders) apply(m Message, headers http.Header, verify bool) error {
	if m.method != MethodCallTool {
		return nil
	}
	arguments, _ := decodeObject(m.params["arguments"])
	for _, parameter := range h.params {
		if err := parameter.apply(arguments, headers, verify); err != nil {
			return err
		}
	}
	return nil
}

func (p parameterHeader) apply(arguments object, headers http.Header, verify bool) error {
	raw := parameterValue(arguments, p.path)
	if raw == nil || bytes.Equal(raw, []byte("null")) {
		if verify && len(headerValues(headers, p.name)) != 0 {
			return fmt.Errorf("unexpected %s header", p.name)
		}
		return nil
	}
	value, ok := p.convert(raw)
	if !ok {
		return fmt.Errorf("invalid value for %s", p.name)
	}
	if verify {
		if !p.matches(headers, value) {
			return fmt.Errorf("missing, malformed or mismatched %s header", p.name)
		}
		return nil
	}
	headers.Set(p.name, EncodeHeaderValue(value))
	return nil
}

func (p parameterHeader) matches(headers http.Header, want string) bool {
	if p.kind != schemaInteger {
		return headerMatches(headers, p.name, want, true)
	}
	values := headerValues(headers, p.name)
	if len(values) != 1 {
		return false
	}
	value, err := DecodeHeaderValue(values[0])
	got, ok := safeInteger([]byte(value))
	return err == nil && ok && got == want
}

func (p parameterHeader) convert(value []byte) (string, bool) {
	switch p.kind {
	case schemaString:
		return decodeString(value)
	case schemaBoolean:
		return string(value), optionalBool(value)
	default:
		return safeInteger(value)
	}
}

func parameterValue(arguments object, path []string) json.RawMessage {
	for _, key := range path[:len(path)-1] {
		arguments, _ = decodeObject(arguments[key])
		if arguments == nil {
			return nil
		}
	}
	return arguments[path[len(path)-1]]
}

func safeInteger(raw []byte) (string, bool) {
	if !isNumber(raw) {
		return "", false
	}
	digits, exponent, negative := integerParts(string(raw))
	if digits == "" {
		return "0", true
	}
	if exponent < 0 || exponent > 16-len(digits) {
		return "", false
	}
	decimal := digits + strings.Repeat("0", exponent)
	if _, err := strconv.ParseUint(decimal, 10, 53); err != nil {
		return "", false
	}
	if negative {
		return "-" + decimal, true
	}
	return decimal, true
}

func integerParts(value string) (digits string, exponent int, negative bool) {
	value, negative = strings.CutPrefix(value, "-")
	mantissa, scientific := value, "0"
	if index := strings.IndexAny(value, "eE"); index >= 0 {
		mantissa, scientific = value[:index], value[index+1:]
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	significand := strings.TrimLeft(whole+fraction, "0")
	digits = strings.TrimRight(significand, "0")
	exponent, err := strconv.Atoi(scientific)
	if err != nil || exponent > len(value)+16 || exponent < -len(value) {
		return digits, -1, negative
	}
	return digits, exponent - len(fraction) + len(significand) - len(digits), negative
}

// CredentialHeader reports whether a configuration may supply the named
// header without overriding MCP routing, authority or HTTP framing.
func CredentialHeader(name string) bool {
	lower := strings.ToLower(name)
	if !fieldToken(name) || strings.HasPrefix(lower, "mcp-") || strings.HasPrefix(lower, "sec-") ||
		strings.HasPrefix(lower, "proxy-") || strings.HasPrefix(lower, "x-forwarded-") {
		return false
	}
	switch lower {
	case "host", "origin", "accept", "content-type", "content-length", "content-encoding", "connection",
		"keep-alive", "te", "trailer", "transfer-encoding", "upgrade", "last-event-id", "forwarded", "expect":
		return false
	default:
		return true
	}
}
