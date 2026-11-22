package mcpwire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestMRTRPinnedRequestShapes(t *testing.T) {
	const form = `{"mode":"form","message":"Choose","requestedSchema":{"type":"object","properties":{"choice":{"type":"array","items":{"anyOf":[{"const":"yes","title":"Yes"}]}}},"required":["choice"]}}`
	const sampling = `{"messages":[{"role":"user","content":{"type":"text","text":"Summarize","vendor":{"size":900719925474099312345}}}],"maxTokens":64}`
	const tools = `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call","name":"echo","input":{}}]},{"role":"user","content":[{"type":"tool_result","toolUseId":"call","content":[{"type":"text","text":"ok"}]}]}],"maxTokens":64,"tools":[{"name":"echo","inputSchema":{"type":"object"}}],"toolChoice":{"mode":"required"}}`
	const elicitationMethod = "elicitation/create"
	const samplingMethod = "sampling/createMessage"
	for _, tc := range []struct {
		name, caps, method, params string
		valid                      bool
	}{
		{"URL without ID", `{"elicitation":{"url":{}}}`, elicitationMethod, `{"mode":"url","message":"Open","url":"https://example.org/continue","vendor":{"keep":true}}`, true},
		{"URL without capability", `{"elicitation":{"form":{}}}`, elicitationMethod, `{"mode":"url","message":"Open","url":"https://example.org/continue"}`, false},
		{"URL without URL", `{"elicitation":{"url":{}}}`, elicitationMethod, `{"mode":"url","message":"Open","elicitationId":"old"}`, false},
		{"URL relative", `{"elicitation":{"url":{}}}`, elicitationMethod, `{"mode":"url","message":"Open","url":"/continue"}`, false},
		{"form restricted schema", `{"elicitation":{"form":{}}}`, elicitationMethod, form, true},
		{"implicit form", `{"elicitation":{}}`, elicitationMethod, strings.Replace(form, `"mode":"form",`, "", 1), true},
		{"empty form mode", `{"elicitation":{}}`, elicitationMethod, strings.Replace(form, `"mode":"form"`, `"mode":""`, 1), false},
		{"missing form properties", `{"elicitation":{}}`, elicitationMethod, `{"message":"Open","requestedSchema":{"type":"object"}}`, false},
		{"nested form property", `{"elicitation":{}}`, elicitationMethod, `{"message":"Open","requestedSchema":{"type":"object","properties":{"nested":{"type":"object","properties":{}}}}}`, false},
		{"invalid form option", `{"elicitation":{}}`, elicitationMethod, strings.Replace(form, `,"title":"Yes"`, "", 1), false},
		{"sampling block and vendor field", `{"sampling":{}}`, samplingMethod, sampling, true},
		{"sampling text vendor metadata", `{"sampling":{}}`, samplingMethod, strings.Replace(sampling, `"type":"text"`, `"type":"text","_meta":{"vendor":true}`, 1), true},
		{"sampling text scalar metadata", `{"sampling":{}}`, samplingMethod, strings.Replace(sampling, `"type":"text"`, `"type":"text","_meta":false`, 1), false},
		{"sampling tool use scalar metadata", `{"sampling":{"tools":{}}}`, samplingMethod, strings.Replace(tools, `"type":"tool_use"`, `"type":"tool_use","_meta":null`, 1), false},
		{"sampling tool result scalar metadata", `{"sampling":{"tools":{}}}`, samplingMethod, strings.Replace(tools, `"type":"tool_result"`, `"type":"tool_result","_meta":"invalid"`, 1), false},
		{"sampling tool result child scalar metadata", `{"sampling":{"tools":{}}}`, samplingMethod, strings.Replace(tools, `"type":"text"`, `"type":"text","_meta":[]`, 1), false},
		{"sampling content object empty", `{"sampling":{}}`, samplingMethod, `{"messages":[{"role":"user","content":{}}],"maxTokens":64}`, false},
		{"sampling content array has invalid member", `{"sampling":{}}`, samplingMethod, `{"messages":[{"role":"user","content":[{"type":"text","text":"ok"},{"type":"image","data":"AA=="}]}],"maxTokens":64}`, false},
		{"sampling content unknown discriminator", `{"sampling":{}}`, samplingMethod, `{"messages":[{"role":"user","content":{"type":"vendor","text":"ok"}}],"maxTokens":64}`, false},
		{"sampling tools", `{"sampling":{"tools":{}}}`, samplingMethod, tools, true},
		{"sampling tools without capability", `{"sampling":{}}`, samplingMethod, tools, false},
		{"sampling tool result empty", `{"sampling":{"tools":{}}}`, samplingMethod, strings.Replace(tools, `"toolUseId":"call",`, "", 1), false},
	} {
		req := mustParse(t, requestBytes("1", MethodCallTool, Version, tc.caps, `,"name":"echo"`))
		result := envResult("1", `{"resultType":"input_required","inputRequests":{"more":{"method":"`+tc.method+`","params":`+tc.params+`}}}`)
		t.Run(tc.name, func(t *testing.T) { assertMRTRWireResult(t, req, result, tc.valid) })
	}
}

func assertMRTRWireResult(t *testing.T, req Message, raw []byte, valid bool) {
	t.Helper()
	for _, transport := range []ReplyTransport{MeshReply, StdioReply, HTTPReply} {
		event, err := NewExchange(req, transport).Accept(raw)
		if !valid && errors.Is(err, ErrPeerMessage) {
			continue
		}
		if !valid || err != nil || !event.Terminal || !bytes.Equal(event.Message.Raw(), raw) {
			t.Errorf("transport %d accepted=%v with %v, want valid=%v: %s", transport, event.Terminal, err, valid, raw)
		}
	}
}

func TestOversizedStdioReplyAttribution(t *testing.T) {
	big := strings.Repeat("x", MaxMessage+64)
	const version = `{"jsonrpc":"2.0",`
	const result = `"result":{"text":"`
	next := envResult("1", `{"resultType":"complete","content":[]}`)
	const largeID = "900719925474099312345"
	for _, tc := range []struct {
		name, line, id string
		fault          bool
	}{
		{"numeric ID", version + `"id":` + largeID + "," + result + big + `"}}`, largeID, false},
		{"string ID follows large payload", version + result + big + `"},"id":"\u0078"}`, `"\u0078"`, false},
		{"huge compound value", version + `"result":{"parts":["` + big + `","` + big + `"]},"id":8}`, "8", false},
		{"escaped duplicate ID", version + `"id":1,` + result + big + `"},"\u0069d":2}`, "", true},
		{"nested ID only", version + result + big + `","id":1}}`, "", true},
		{"extra value", version + `"id":1,` + result + big + `"}}{}`, "", true},
		{"malformed suffix", version + `"id":1,` + result + big + `"},`, "", true},
		{"request instead of response", version + `"id":1,"method":"tools/call","params":{"text":"` + big + `"}}`, "", true},
	} {
		reader := NewStdioReader(io.MultiReader(strings.NewReader(tc.line+"\r\n"), bytes.NewReader(append(next, '\n'))))
		t.Run(tc.name, func(t *testing.T) { assertOversizeReply(t, reader, next, tc.id, tc.fault) })
	}
	reader := NewStdioReader(strings.NewReader(version + `"id":1,` + result + big + `"}}`))
	if _, err := reader.ReadReply(); !errors.Is(err, ErrFramingLost) || !errors.Is(err, io.EOF) {
		t.Fatalf("oversized reply without line delimiter: %v", err)
	}
}

func assertOversizeReply(t *testing.T, reader *StdioReader, next []byte, wantID string, fault bool) {
	t.Helper()
	raw, err := reader.ReadReply()
	var oversized *OversizedStdioReply
	attributed := errors.As(err, &oversized)
	if raw != nil || !errors.Is(err, ErrTooLarge) || errors.Is(err, ErrFramingLost) != fault || attributed == fault {
		t.Fatalf("oversized reply raw length %d, err=%v, attributed=%v, want fault=%v", len(raw), err, attributed, fault)
	}
	if attributed {
		want, _ := ParseID([]byte(wantID))
		if !want.Equal(oversized.ID) || oversized.ID.raw != wantID {
			t.Fatalf("response ID = %q, want %q", oversized.ID.raw, wantID)
		}
	}
	if raw, err := reader.ReadReply(); err != nil || !bytes.Equal(raw, next) {
		t.Fatalf("following frame = %q, %v", raw, err)
	}
}

func TestOversizedStdioReplyFromPrebufferedInput(t *testing.T) {
	line := `{"jsonrpc":"2.0","result":{"text":"` + strings.Repeat("x", MaxMessage+64) + `"},"id":"match"}`
	next := envResult("1", `{"resultType":"complete","content":[]}`)
	source := io.MultiReader(strings.NewReader(line+"\r\n"), bytes.NewReader(append(next, '\n')))
	reader := NewStdioReader(bufio.NewReaderSize(source, len(line)+2))
	assertOversizeReply(t, reader, next, `"match"`, false)
}

func TestSSEReadFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name, tail string
		readErr    error
		want       error
	}{
		{"unexpected EOF", "data: {", io.ErrUnexpectedEOF, ErrInterrupted},
		{"truncated UTF-8", "data: \xe2\x82", io.ErrUnexpectedEOF, ErrInterrupted},
		{"custom disconnect", "", io.ErrClosedPipe, ErrInterrupted},
		{"clean EOF before completion", "", io.EOF, ErrInterrupted},
		{"incomplete event with EOF", "data: {\n", io.EOF, ErrInterrupted},
		{"malformed SSE event", "event: endpoint\ndata: {}\n\n", io.EOF, ErrPeerMessage},
		{"malformed JSON event", "data: {\n\n", io.EOF, ErrPeerMessage},
		{"malformed UTF-8 event", "data: \xff\n\n", io.EOF, ErrPeerMessage},
		{"oversized event", "data: " + strings.Repeat("x", MaxMessage+1) + "\n\n", io.EOF, ErrTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) { assertSSEReadFailure(t, tc.tail, tc.readErr, tc.want) })
	}
}

func assertSSEReadFailure(t *testing.T, tail string, readErr, want error) {
	t.Helper()
	m := fixtureRequest(t, "7", MethodListen, `,"notifications":{"toolsListChanged":true}`)
	ack := subNotification("subscriptions/acknowledged", "7", `,"notifications":{"toolsListChanged":true}`)
	res := testHTTPResponse(200, sseMedia, "")
	defer func() { _ = res.Body.Close() }()
	res.Body = io.NopCloser(io.MultiReader(strings.NewReader("data: "+string(ack)+"\n\n"+tail), iotest.ErrReader(readErr)))
	reader, err := OpenHTTPResponse(m, res)
	if err != nil {
		t.Fatal(err)
	}
	event, err := reader.Next()
	if err != nil || event.Terminal || !bytes.Equal(event.Message.Raw(), ack) {
		t.Fatalf("SSE acknowledgement = %+v, %v", event, err)
	}
	_, err = reader.Next()
	if !errors.Is(err, want) || errors.Is(err, ErrInterrupted) != errors.Is(want, ErrInterrupted) {
		t.Fatalf("SSE classification = %v, want %v", err, want)
	}
	if !errors.Is(readErr, io.EOF) && !errors.Is(err, readErr) {
		t.Fatalf("SSE read cause %v missing from %v", readErr, err)
	}
}

func TestCapabilityErrorOnlyReportsMissingLeaves(t *testing.T) {
	methods := Methods{MethodCallTool: {
		"roots":       map[string]any{"listChanged": true},
		"elicitation": map[string]any{"form": map[string]any{}, "url": map[string]any{}},
		"extensions":  map[string]any{"vendor.test/capability": map[string]any{"nested": map[string]any{"first": "supported", "second": map[string]any{}}}},
	}}
	const want = `{"elicitation":{"url":{}},"extensions":{"vendor.test/capability":{"nested":{"second":{}}}}}`
	for _, modes := range []string{`{"form":{}}`, `{}`} {
		caps := `{"roots":{"listChanged":true},"elicitation":` + modes + `,"extensions":{"vendor.test/capability":{"nested":{"first":"supported"}}}}`
		raw := requestBytes("1", MethodCallTool, Version, caps, `,"name":"echo"`)
		t.Run(modes, func(t *testing.T) { assertCapabilityErrorOnBothTransports(t, raw, methods, want) })
	}
}

func assertCapabilityErrorOnBothTransports(t *testing.T, raw []byte, methods Methods, want string) {
	t.Helper()
	assertMissingSubset(t, StdioInput(raw, methods).Fault, want)
	m := mustParse(t, raw)
	headers, err := BuildHTTPHeaders(m, ParameterHeaders{})
	if err != nil {
		t.Fatal(err)
	}
	r := testPOST(raw)
	r.Header = headers
	in, err := ReadHTTP(r, HTTPPolicy{Methods: methods})
	assertHTTPInput(t, in, err, 400, MissingCapability)
	assertMissingSubset(t, in.Fault, want)
}

func assertMissingSubset(t *testing.T, fault *Fault, want string) {
	t.Helper()
	if fault == nil || fault.Code != MissingCapability {
		t.Fatalf("fault = %+v", fault)
	}
	wire, err := fault.Reply()
	if err != nil {
		t.Fatal(err)
	}
	m := mustParse(t, wire)
	rawError, _ := decodeObject(m.ErrorObject())
	data, _ := decodeObject(rawError["data"])
	if string(data["requiredCapabilities"]) != want {
		t.Fatalf("requiredCapabilities = %s, want %s", data["requiredCapabilities"], want)
	}
}

func TestMirroredHeaderSchemaPositions(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"x-mcp-header":{"type":"string","x-mcp-header":"Literal-Property-Name"},
			"q":{"type":"string","x-mcp-header":"Query","default":{"x-mcp-header":"literal"}}
		},
		"examples":[{"x-mcp-header":"literal","properties":{"q":{"x-mcp-header":"literal"}}}],
		"enum":[{"x-mcp-header":"literal"}],
		"const":{"x-mcp-header":"literal"},
		"x-vendor":{"x-mcp-header":"literal"}
	}`)
	mirror, err := CompileParameterHeaders(schema)
	if err != nil {
		t.Fatal(err)
	}
	m := fixtureRequest(t, "1", MethodCallTool, `,"name":"echo","arguments":{"x-mcp-header":"value","q":"search"}`)
	headers, err := BuildHTTPHeaders(m, mirror)
	if err != nil || headers.Get("Mcp-Param-Literal-Property-Name") != "value" || headers.Get("Mcp-Param-Query") != "search" {
		t.Fatalf("mirrored headers = %v, %v", headers, err)
	}
	for _, raw := range []string{
		`{"type":"object","prefixItems":[{"type":"string","x-mcp-header":"invalid"}]}`,
		`{"type":"object","dependentSchemas":{"q":{"properties":{"value":{"type":"string","x-mcp-header":"invalid"}}}}}`,
		`{"type":"object","$defs":{"x-mcp-header":{"type":"string","x-mcp-header":"invalid"}}}`,
	} {
		if _, err := CompileParameterHeaders(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted annotation in unreachable subschema: %s", raw)
		}
	}
}

func TestSSEReadFailureAfterTerminalReply(t *testing.T) {
	req := fixtureRequest(t, "7", MethodListTools, "")
	result := envResult("7", `{"resultType":"complete","tools":[],"ttlMs":0,"cacheScope":"private"}`)
	res := testHTTPResponse(200, sseMedia, "")
	defer func() { _ = res.Body.Close() }()
	res.Body = io.NopCloser(io.MultiReader(strings.NewReader("data: "+string(result)+"\n\n"), iotest.ErrReader(io.ErrUnexpectedEOF)))
	reader, err := OpenHTTPResponse(req, res)
	if err != nil {
		t.Fatal(err)
	}
	if event, err := reader.Next(); err != nil || !event.Terminal {
		t.Fatalf("terminal SSE event = %+v, %v", event, err)
	}
	if _, err := reader.Next(); !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, ErrInterrupted) {
		t.Fatalf("post-terminal SSE error = %v", err)
	}
}
