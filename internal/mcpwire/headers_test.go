package mcpwire

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParameterHeaders(t *testing.T) {
	const raw = `{"type":"object","properties":{
		"nested":{"type":"object","properties":{"name":{"type":"string","x-mcp-header":"Nested"}}},
		"id":{"type":"integer","x-mcp-header":"ID"},
		"enabled":{"type":"boolean","x-mcp-header":"Enabled"},
		"absent":{"type":"string","x-mcp-header":"Absent"},
		"null":{"type":"string","x-mcp-header":"Null"}
	}}`
	headers, err := CompileParameterHeaders(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	req := fixtureRequest(t, "1", "tools/call", `,"name":"fø","arguments":{"nested":{"name":" =?base64?x?= "},"id":42.0,"enabled":false,"null":null}`)
	wire, err := BuildHTTPHeaders(req, headers)
	if err != nil {
		t.Fatal(err)
	}
	if wire.Get("Mcp-Param-Id") != "42" || wire.Get("Mcp-Param-Enabled") != "false" ||
		wire.Get(NameHeader) != "=?base64?ZsO4?=" || wire.Get("Mcp-Param-Null") != "" ||
		wire.Get("Mcp-Param-Absent") != "" {
		t.Fatalf("unexpected headers: %v", wire)
	}
	rt := testPOST(req.Raw())
	rt.Header = wire.Clone()
	rt.Header.Set("Mcp-Param-ID", "42.0")
	rt.Header.Set("Mcp-Session-Id", "old")
	rt.Header.Set("Last-Event-ID", "resume")
	rt.Header.Set("Mcp-Param-Unknown", "ignored")
	policy := HTTPPolicy{Methods: fixtureMethods, ToolHeaders: func(string) ParameterHeaders { return headers }}
	in, err := ReadHTTP(rt, policy)
	assertHTTPInput(t, in, err, 200, 0)

	for _, tc := range []struct{ field, value string }{
		{"Mcp-Param-Enabled", "true"},
		{"Mcp-Param-Nested", "=?base64?wA==?="},
		{"Mcp-Param-Nested", ""},
		{"Mcp-Param-Id", "9007199254740992"},
		{"Mcp-Param-Null", "unexpected"},
	} {
		rt := testPOST(req.Raw())
		rt.Header = wire.Clone()
		if tc.value == "" {
			rt.Header.Del(tc.field)
		} else {
			rt.Header.Set(tc.field, tc.value)
		}
		in, err := ReadHTTP(rt, policy)
		assertHTTPInput(t, in, err, 400, HeaderMismatch)
	}
}

func TestRejectInvalidParameterSchemas(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object","x-mcp-header":"Root"}`,
		`{"type":"object","properties":{"x":{"type":"number","x-mcp-header":"X"}}}`,
		`{"type":"object","properties":{"x":{"type":"boolean","x-mcp-header":""}}}`,
		`{"type":"object","properties":{"x":{"type":"string","x-mcp-header":"bad\r\n"}}}`,
		`{"type":"object","properties":{"x":{"type":"string","x-mcp-header":"A"},"nested":{"properties":{"a":{"type":"string","x-mcp-header":"a"}}}}}`,
		`{"type":"object","properties":{"x":{"type":"array","items":{"type":"string","x-mcp-header":"X"}}}}`,
		`{"type":"object","properties":{"x":{"allOf":[{"type":"string","x-mcp-header":"X"}]}}}`,
		`{"type":"object","properties":{"x":{"if":{"type":"string","x-mcp-header":"X"}}}}`,
		`{"type":"object","properties":{"x":{"$ref":"#/$defs/x"}},"$defs":{"x":{"type":"string","x-mcp-header":"X"}}}`,
	} {
		if _, err := CompileParameterHeaders(json.RawMessage(schema)); err == nil {
			t.Errorf("accepted %s", schema)
		}
	}
}

func TestCredentialHeaders(t *testing.T) {
	for _, name := range []string{"Host", "Origin", "MCP-Protocol-Version", "mcp-name", "Mcp-Session-Id",
		"Mcp-Param-X-Key", "Last-Event-ID", "X-Forwarded-Host", "Connection", "Transfer-Encoding", "Accept", "bad name"} {
		if CredentialHeader(name) {
			t.Errorf("allowed reserved header %q", name)
		}
	}
	for _, name := range []string{"Authorization", "X-API-Key", "Cookie"} {
		if !CredentialHeader(name) {
			t.Errorf("denied credential header %q", name)
		}
	}
}

func FuzzHeaderEncoding(f *testing.F) {
	f.Add("=?base64?literal?=")
	f.Add("=?base64?=")
	f.Add(" 東\t\r\n ")
	f.Add("plain")
	f.Fuzz(func(t *testing.T, value string) {
		value = strings.ToValidUTF8(value, "�")
		wire := EncodeHeaderValue(value)
		decoded, err := DecodeHeaderValue(wire)
		if err != nil || decoded != value || !plainHeaderValue(wire) {
			t.Fatalf("roundtrip %q -> %q -> %q: %v", value, wire, decoded, err)
		}
		if decoded, err := DecodeHeaderValue(value); err == nil && !utf8.ValidString(decoded) {
			t.Fatalf("decoded invalid UTF-8: %q", decoded)
		}
	})
}
