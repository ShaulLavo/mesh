package mcphost

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/shaul/mesh/internal/mcpwire"
)

func compileFor(t *testing.T, schema string) mcpwire.ParameterHeaders {
	t.Helper()
	listed, err := CompileToolHeaders(json.RawMessage(`{"tools":[{"name":"t","inputSchema":` + schema + `}]}`))
	if err != nil {
		t.Fatal(err)
	}
	headers, ok := listed.Lookup("t")
	if !ok {
		t.Fatalf("schema rejected: %v", listed.Rejected)
	}
	return headers
}

// mismatchServer rejects a tool call whose Mcp-Param-Region header does not
// mirror arguments.region, before running anything.
func mismatchServer(t *testing.T, ran *atomic.Int32) (string, *recorder) {
	t.Helper()
	rec := &recorder{}
	return startServer(t, rec.handler(func(w http.ResponseWriter, r *http.Request, body []byte) {
		id := requestID(t, body)
		var call struct {
			Params struct {
				Arguments struct {
					Region string `json:"region"`
				} `json:"arguments"`
			} `json:"params"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &call)
		want := call.Params.Arguments.Region
		got, err := mcpwire.DecodeHeaderValue(r.Header.Get("Mcp-Param-Region"))
		switch {
		case call.Method == "tools/list" && strings.Contains(string(body), `"force-mismatch"`):
			writeJSON(w, http.StatusBadRequest, errorFrame(id, mcpwire.HeaderMismatch, "mismatch"))
		case call.Method == "tools/call" && (err != nil || got != want):
			writeJSON(w, http.StatusBadRequest, errorFrame(id, mcpwire.HeaderMismatch, "Mcp-Param-Region does not match"))
		default:
			ran.Add(1)
			writeJSON(w, http.StatusOK, okResult(id))
		}
	})), rec
}

const regionSchema = `{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}`

func TestHeaderMismatchThenRefreshAndOneRetry(t *testing.T) {
	var ran atomic.Int32
	endpoint, rec := mismatchServer(t, &ran)
	var schema atomic.Pointer[mcpwire.ParameterHeaders]
	upstream := dialUpstream(t, endpoint, nil, func(string) (mcpwire.ParameterHeaders, bool) {
		if known := schema.Load(); known != nil {
			return *known, true
		}
		return mcpwire.ParameterHeaders{}, false
	})
	m := toolCall(t, "1", "t", `{"region":"eu-west"}`, "")

	events, err := callAll(t, upstream, m)
	if !errors.Is(err, ErrHeaderMismatch) || len(events) != 0 || ran.Load() != 0 {
		t.Fatalf("stale schema: events %d, err %v, ran %d", len(events), err, ran.Load())
	}
	headers := compileFor(t, regionSchema)
	schema.Store(&headers)
	ctx := WithSchemaRefreshed(testContext(t))
	var got []mcpwire.Event
	if err := upstream.Call(ctx, m, func(e mcpwire.Event) error { got = append(got, e); return nil }); err != nil || terminals(got) != 1 {
		t.Fatalf("retry: %v", err)
	}
	requests := rec.all()
	if mirrored := requests[len(requests)-1].Header.Get("Mcp-Param-Region"); mirrored != "eu-west" {
		t.Fatalf("mirrored header %q", mirrored)
	}

	// A second mismatch on the retry is the caller's result, not another refresh.
	schema.Store(&mcpwire.ParameterHeaders{})
	got = nil
	if err := upstream.Call(ctx, m, func(e mcpwire.Event) error { got = append(got, e); return nil }); err != nil || len(got) != 1 {
		t.Fatalf("second mismatch: %v %d", err, len(got))
	}
	if code := mcpErrorCode(t, got[0]); code != mcpwire.HeaderMismatch {
		t.Fatalf("delivered code %d", code)
	}
}

func mcpErrorCode(t *testing.T, event mcpwire.Event) int {
	t.Helper()
	var failure struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(event.Message.ErrorObject(), &failure); err != nil {
		t.Fatalf("not an error: %s", event.Message.Raw())
	}
	return failure.Code
}

func TestOnlyAConfirmedHeaderMismatchRefreshes(t *testing.T) {
	var ran atomic.Int32
	endpoint, _ := mismatchServer(t, &ran)
	other := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "invalid-params"):
			writeJSON(w, http.StatusBadRequest, errorFrame(requestID(t, body), mcpwire.InvalidParams, "bad"))
		case strings.Contains(string(body), "internal"):
			startSSE(w)
			writeFrames(w, errorFrame(requestID(t, body), -31000, "boom"))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	tools := func(string) (mcpwire.ParameterHeaders, bool) { return mcpwire.ParameterHeaders{}, false }

	t.Run("not a tool call", func(t *testing.T) {
		upstream := dialUpstream(t, endpoint, nil, tools)
		m := request(t, "1", mcpwire.MethodListTools, `"force-mismatch":true`, "")
		events, err := callAll(t, upstream, m)
		if err != nil || len(events) != 1 || mcpErrorCode(t, events[0]) != mcpwire.HeaderMismatch {
			t.Fatalf("events %d, err %v", len(events), err)
		}
	})
	t.Run("other 400 error", func(t *testing.T) {
		upstream := dialUpstream(t, other, nil, tools)
		events, err := callAll(t, upstream, toolCall(t, "1", "t", `{"x":"invalid-params"}`, ""))
		if err != nil || len(events) != 1 || mcpErrorCode(t, events[0]) != mcpwire.InvalidParams {
			t.Fatalf("events %d, err %v", len(events), err)
		}
	})
	t.Run("error inside a stream", func(t *testing.T) {
		upstream := dialUpstream(t, other, nil, tools)
		events, err := callAll(t, upstream, toolCall(t, "1", "t", `{"x":"internal"}`, ""))
		if err != nil || len(events) != 1 || mcpErrorCode(t, events[0]) != -31000 {
			t.Fatalf("events %d, err %v", len(events), err)
		}
	})
	t.Run("bare 400 is ambiguous", func(t *testing.T) {
		upstream := dialUpstream(t, other, nil, tools)
		_, err := callAll(t, upstream, toolCall(t, "1", "t", `{}`, ""))
		var status *HTTPStatusError
		if errors.Is(err, ErrHeaderMismatch) || !errors.As(err, &status) || !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("err %v", err)
		}
	})
}

func TestUnmirrorableArgumentsAreNeverSent(t *testing.T) {
	var ran atomic.Int32
	endpoint, rec := mismatchServer(t, &ran)
	headers := compileFor(t, `{"type":"object","properties":{"n":{"type":"integer","x-mcp-header":"N"}}}`)
	upstream := dialUpstream(t, endpoint, nil, func(string) (mcpwire.ParameterHeaders, bool) { return headers, true })
	_, err := callAll(t, upstream, toolCall(t, "1", "t", `{"n":1e30}`, ""))
	if !errors.Is(err, ErrNotSent) || len(rec.all()) != 0 {
		t.Fatalf("err %v, requests %d", err, len(rec.all()))
	}
}

func TestCompileToolHeadersRejectsBadAnnotations(t *testing.T) {
	good := `{"name":"good","inputSchema":{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"A"},` +
		`"nested":{"type":"object","properties":{"b":{"type":"integer","x-mcp-header":"B"},"c":{"type":"boolean","x-mcp-header":"C"}}}}}}`
	rejected := map[string]string{
		"unreachable under anyOf":   `{"type":"object","anyOf":[{"properties":{"p":{"type":"string","x-mcp-header":"P"}}}]}`,
		"under items":               `{"type":"object","properties":{"l":{"type":"array","items":{"type":"string","x-mcp-header":"L"}}}}`,
		"under definitions":         `{"type":"object","$defs":{"d":{"type":"string","x-mcp-header":"D"}}}`,
		"number type":               `{"type":"object","properties":{"p":{"type":"number","x-mcp-header":"P"}}}`,
		"object type":               `{"type":"object","properties":{"p":{"type":"object","x-mcp-header":"P"}}}`,
		"missing type":              `{"type":"object","properties":{"p":{"x-mcp-header":"P"}}}`,
		"empty name":                `{"type":"object","properties":{"p":{"type":"string","x-mcp-header":""}}}`,
		"space in name":             `{"type":"object","properties":{"p":{"type":"string","x-mcp-header":"A B"}}}`,
		"colon in name":             `{"type":"object","properties":{"p":{"type":"string","x-mcp-header":"A:B"}}}`,
		"non ASCII name":            `{"type":"object","properties":{"p":{"type":"string","x-mcp-header":"Ä"}}}`,
		"non string name":           `{"type":"object","properties":{"p":{"type":"string","x-mcp-header":7}}}`,
		"case-insensitive repeat":   `{"type":"object","properties":{"p":{"type":"string","x-mcp-header":"Region"},"q":{"type":"string","x-mcp-header":"rEGION"}}}`,
		"top level annotation":      `{"type":"object","x-mcp-header":"Top"}`,
		"duplicate key in document": `{"type":"object","type":"object"}`,
		"not an object":             `[]`,
	}
	list := `{"tools":[` + good
	for name, schema := range rejected {
		list += `,{"name":` + jsonString(name) + `,"inputSchema":` + schema + `}`
	}
	list += `,{"name":"twin","inputSchema":{"type":"object"}},{"name":"twin","inputSchema":{"type":"object"}}]}`
	compiled, err := CompileToolHeaders(json.RawMessage(list))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := compiled.Lookup("good"); !ok {
		t.Fatalf("valid tool rejected: %v", compiled.Rejected)
	}
	for name := range rejected {
		if _, ok := compiled.Lookup(name); ok {
			t.Errorf("%s was advertised", name)
		}
	}
	if _, ok := compiled.Lookup("twin"); ok {
		t.Error("a duplicated tool name was advertised")
	}
	if len(compiled.Rejected) != len(rejected)+2 {
		t.Errorf("rejected %d, want %d", len(compiled.Rejected), len(rejected)+2)
	}
	if _, err := CompileToolHeaders(json.RawMessage(`{"tools":7}`)); err == nil {
		t.Error("a malformed listing was accepted")
	}
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// FuzzBuildRequestHeaders builds the request headers from an arbitrary name
// and argument values and checks what any server would receive.
func FuzzBuildRequestHeaders(f *testing.F) {
	for _, seed := range []struct{ tool, header, text string }{
		{"echo", "Region", "us-east"}, {"é", "Region", "ünï"}, {"echo", "A", " padded "}, {"echo", "A", "=?base64?AAAA?="},
		{"echo", "A", "line\r\nX-Evil: 1"}, {"echo", "A", ""}, {"=?base64?x?=", "A", "\x00\x7f"}, {"a b", "A-B", "tab\t"},
	} {
		f.Add(seed.tool, seed.header, seed.text)
	}
	endpoint, err := parseHTTPEndpoint("http://127.0.0.1:8080/mcp")
	if err != nil {
		f.Fatal(err)
	}
	client := newHTTPClient(endpoint, []credentialHeader{{name: "Authorization", value: "Bearer s"}})
	f.Fuzz(func(t *testing.T, tool, header, text string) {
		if !utf8.ValidString(tool) || !utf8.ValidString(text) || tool == "" {
			return
		}
		schema, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{
			"p": map[string]any{"type": "string", "x-mcp-header": header}}})
		list, _ := json.Marshal(map[string]any{"tools": []any{map[string]any{"name": tool, "inputSchema": json.RawMessage(schema)}}})
		compiled, err := CompileToolHeaders(list)
		if err != nil {
			t.Fatal(err)
		}
		headers, advertised := compiled.Lookup(tool)
		arguments, _ := json.Marshal(map[string]any{"p": text})
		name, _ := json.Marshal(tool)
		m, fault := mcpwire.Parse([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"_meta":{"` +
			mcpwire.VersionKey + `":"` + mcpwire.Version + `","` + mcpwire.ClientCapsKey + `":{}},"name":` + string(name) + `,"arguments":` + string(arguments) + `}}`))
		if fault != nil {
			return
		}
		req, cancel, err := client.newRequest(t.Context(), httpCall{request: m, toolHeaders: headers})
		if err != nil {
			if !errors.Is(err, ErrNotSent) {
				t.Fatalf("pre-flight failure not marked unsent: %v", err)
			}
			return
		}
		defer cancel()
		assertWireHeaders(t, req.Header, tool, text, header, advertised)
	})
}

func assertWireHeaders(t *testing.T, h http.Header, tool, text, header string, advertised bool) {
	t.Helper()
	for name, values := range h {
		for _, value := range values {
			for i := range len(value) {
				if c := value[i]; (c < 0x20 && c != '\t') || c > 0x7e {
					t.Fatalf("header %s carries byte %#x: %q", name, c, value)
				}
			}
		}
	}
	if got, err := mcpwire.DecodeHeaderValue(h.Get(mcpwire.NameHeader)); err != nil || got != tool {
		t.Fatalf("Mcp-Name %q decodes to %q, %v", h.Get(mcpwire.NameHeader), got, err)
	}
	if h.Get("Authorization") != "Bearer s" || len(h.Values(mcpwire.MethodHeader)) != 1 || len(h.Values(mcpwire.ProtocolHeader)) != 1 {
		t.Fatalf("routing or credential headers damaged: %v", h)
	}
	mirrored := h.Values("Mcp-Param-" + header)
	if !advertised {
		if len(mirrored) != 0 {
			t.Fatalf("mirrored a rejected annotation: %v", mirrored)
		}
		return
	}
	if len(mirrored) != 1 {
		t.Fatalf("mirrored %d values", len(mirrored))
	}
	if got, err := mcpwire.DecodeHeaderValue(mirrored[0]); err != nil || got != text {
		t.Fatalf("mirrored value %q decodes to %q, %v; want %q", mirrored[0], got, err, text)
	}
}
