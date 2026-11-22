package mcpwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

var fixtureMethods = Methods{
	"server/discover": nil, "tools/list": nil, "tools/call": nil, "prompts/list": nil,
	"prompts/get": nil, "resources/list": nil, "resources/templates/list": nil,
	"resources/read": nil, "subscriptions/listen": nil, "completion/complete": nil,
}

const fixtureInfo = `{"name":"mesh-test","version":"1"}`

func requestBytes(id, method, version, caps, extra string) []byte {
	return fmt.Appendf(nil,
		`{"jsonrpc":"2.0","id":%s,"method":%q,"params":{"_meta":{%q:%q,%q:%s}%s}}`,
		id, method, VersionKey, version, ClientCapsKey, caps, extra)
}

func mustParse(t *testing.T, raw []byte) Message {
	t.Helper()
	m, fault := Parse(raw)
	if fault != nil {
		t.Fatal(fault)
	}
	return m
}

func fixtureRequest(t *testing.T, id, method, extra string) Message {
	t.Helper()
	m := mustParse(t, requestBytes(id, method, Version, "{}", extra))
	if fault := ValidateRequest(m, fixtureMethods); fault != nil {
		t.Fatal(fault)
	}
	return m
}

func envResult(id, result string) []byte {
	return fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result)
}

type stdioCase struct {
	name, raw, replyID string
	code               int
}

func TestStdioRequestMatrix(t *testing.T) {
	methods := Methods{
		"server/discover": nil, "tools/call": {"elicitation": map[string]any{"url": map[string]any{}}},
	}
	for _, tc := range []stdioCase{
		{"malformed", `{"jsonrpc":`, "null", ParseError},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"server/discover"}]`, "null", InvalidRequest},
		{"ambiguous keys", `{"jsonrpc":"2.0","id":1,"id":2,"method":"server/discover"}`, "null", InvalidRequest},
		{"invalid utf-8", "{\"jsonrpc\":\"2.0\",\"method\":\"\xff\"}", "null", InvalidRequest},
		{"noninteger ID", `{"jsonrpc":"2.0","id":1.5,"method":"server/discover"}`, "null", InvalidRequest},
		{"null ID", `{"jsonrpc":"2.0","id":null,"method":"server/discover"}`, "null", InvalidRequest},
		{"invalid envelope, exact numeric ID", `{"jsonrpc":"1.0","id":900719925474099312345,"method":"server/discover"}`, "900719925474099312345", InvalidRequest},
		{"legacy initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`, "1", MethodNotFound},
		{"missing meta", `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`, "1", InvalidParams},
		{"ill-typed version", `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":null}}}`, "1", InvalidParams},
		{"ill-typed caps", string(requestBytes("1", "server/discover", Version, "null", "")), "1", InvalidParams},
		{"unsupported revision", string(requestBytes("1", "server/discover", "2025-11-25", "{}", "")), "1", UnsupportedVersion},
		{"removed ping", string(requestBytes("1", "ping", Version, "{}", "")), "1", MethodNotFound},
		{"extension request", string(requestBytes("1", "tasks/list", Version, `{"extensions":{"io.modelcontextprotocol/tasks":{}}}`, "")), "1", MethodNotFound},
		{"missing nested capability", string(requestBytes("1", "tools/call", Version, `{"elicitation":{}}`, `,"name":"fixture"`)), "1", MissingCapability},
		{"discover", string(requestBytes("1", "server/discover", Version, "{}", "")), "", 0},
		{"capability present", string(requestBytes("1", "tools/call", Version, `{"elicitation":{"url":{}}}`, `,"name":"fixture"`)), "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) { assertStdioRequest(t, tc, methods) })
	}
}

func assertStdioRequest(t *testing.T, tc stdioCase, methods Methods) {
	t.Helper()
	in := StdioInput([]byte(tc.raw), methods)
	if tc.code == 0 && in.Fault == nil && !in.Ignored {
		return
	}
	if in.Fault == nil || in.Fault.Code != tc.code {
		t.Fatalf("fault = %+v, want %d", in.Fault, tc.code)
	}
	raw, err := in.Fault.Reply()
	if err != nil {
		t.Fatal(err)
	}
	reply := mustParse(t, raw)
	if string(reply.fields["id"]) != tc.replyID {
		t.Fatalf("fault reply = %s", raw)
	}
	if _, err := validateRPCError(reply); err != nil {
		t.Fatal(err)
	}
	if tc.name == "legacy initialize" && !bytes.Contains(raw, []byte(Version)) {
		t.Fatalf("revision missing from %s", raw)
	}
}

func TestStdioNotificationsAndActiveIDs(t *testing.T) {
	const cancellation = `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"7","reason":"fixture"}}`
	in := StdioInput([]byte(cancellation), fixtureMethods)
	stringID, _ := ParseID([]byte(`"7"`))
	if in.Fault != nil || in.Ignored || !in.Cancellation.Equal(stringID) {
		t.Fatalf("cancellation = %+v", in)
	}
	for _, discard := range []string{
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":null}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"vendor/event","params":{"secret":"fixture"}}`,
		`{"jsonrpc":"2.0","id":7,"result":{"resultType":"complete"}}`,
	} {
		in := StdioInput([]byte(discard), fixtureMethods)
		if !in.Ignored || in.Fault != nil || in.Cancellation.raw != "" {
			t.Fatalf("expected ignored message: %+v", in)
		}
	}

	numeric := fixtureRequest(t, "7", "server/discover", "")
	text := fixtureRequest(t, `"\u0037"`, "server/discover", "")
	var active ActiveIDs
	if fault := active.Begin(numeric); fault != nil {
		t.Fatal(fault)
	}
	var entered atomic.Int32
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() { tryActiveID(t, &active, text, &entered) })
	}
	group.Wait()
	if entered.Load() != 1 || !text.ID().Equal(stringID) || numeric.ID().Equal(stringID) {
		t.Fatalf("unexpected ID arbitration: successes=%d", entered.Load())
	}
	active.End(stringID)
	if fault := active.Begin(text); fault != nil {
		t.Fatal(fault)
	}
	if fault := active.Begin(numeric); fault == nil {
		t.Fatal("ending string ID also removed numeric ID")
	}
}

func tryActiveID(t *testing.T, active *ActiveIDs, text Message, entered *atomic.Int32) {
	t.Helper()
	fault := active.Begin(text)
	if fault == nil {
		entered.Add(1)
		return
	}
	if fault.Code != InvalidRequest || fault.ID.raw != "" {
		t.Errorf("duplicate unexpectedly correlates to an active request: %+v", fault)
	}
}

func TestStdioBounds(t *testing.T) {
	exact := strings.Repeat(" ", MaxMessage)
	input := NewStdioReader(io.MultiReader(strings.NewReader(exact+"\r\n"), strings.NewReader(exact+"x\n{}\n")))
	if line, err := input.Read(); err != nil || len(line) != MaxMessage {
		t.Fatalf("exact: len=%d err=%v", len(line), err)
	}
	if _, err := input.Read(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized line: %v", err)
	}
	if line, err := input.Read(); err != nil || string(line) != "{}" {
		t.Fatalf("resynchronized: %q, %v", line, err)
	}
	unterminated := NewStdioReader(strings.NewReader("{}"))
	if _, err := unterminated.Read(); !errors.Is(err, ErrFramingLost) {
		t.Fatalf("unterminated line: %v", err)
	}
	beyond := io.LimitReader(repeatingSpace{}, MaxMessage+MaxStdioDrain+1)
	if _, err := NewStdioReader(beyond).Read(); !errors.Is(err, ErrFramingLost) {
		t.Fatalf("drain limit: %v", err)
	}
}

type repeatingSpace struct{}

func (repeatingSpace) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

func TestCompleteCacheHints(t *testing.T) {
	tests := []struct{ method, params, result string }{
		{"server/discover", "", `{"capabilities":{}}`},
		{"tools/list", "", `{"tools":[]}`},
		{"prompts/list", "", `{"prompts":[]}`},
		{"resources/list", "", `{"resources":[]}`},
		{"resources/templates/list", "", `{"resourceTemplates":[]}`},
		{"resources/read", `,"uri":"mesh://test"`, `{"contents":[]}`},
		{"resources/read", `,"uri":"mesh://test","requestState":"opaque"`, `{"contents":[]}`},
	}
	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) { assertCacheHints(t, tc.method, tc.params, tc.result) })
	}
}

func assertCacheHints(t *testing.T, method, params, result string) {
	t.Helper()
	m := fixtureRequest(t, `"\u0037"`, method, params)
	raw, err := Complete(m, json.RawMessage(result), json.RawMessage(fixtureInfo), 30000)
	if err != nil {
		t.Fatal(err)
	}
	event, err := NewExchange(m, StdioReply).Accept(raw)
	if err != nil || !event.Terminal || event.Message.ID().raw != `"\u0037"` {
		t.Fatalf("reply = %s, err=%v", raw, err)
	}
	fields, _ := decodeObject(event.Message.Result())
	ttl := "30000"
	if m.continuation() {
		ttl = "0"
	}
	meta, _ := resultMeta(fields)
	if string(fields["ttlMs"]) != ttl || string(fields["cacheScope"]) != `"private"` ||
		string(meta[ServerInfoKey]) != fixtureInfo {
		t.Fatalf("cache hints or server identity missing: %s", raw)
	}
}

func TestBadMessagesAfterDiscovery(t *testing.T) {
	for _, transport := range []ReplyTransport{MeshReply, StdioReply, HTTPReply} {
		discover := fixtureRequest(t, "1", "server/discover", "")
		good, err := Complete(discover, json.RawMessage(`{"capabilities":{"tools":{}}}`), json.RawMessage(fixtureInfo), 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewExchange(discover, transport).Accept(good); err != nil {
			t.Fatal(err)
		}
		request := fixtureRequest(t, "7", "tools/list", "")
		checkBadMessages(t, NewExchange(request, transport))
		raw, err := Complete(request, json.RawMessage(`{"tools":[]}`), json.RawMessage(fixtureInfo), 0)
		if err != nil {
			t.Fatal(err)
		}
		x := NewExchange(request, transport)
		if _, err := x.Accept(raw); err != nil {
			t.Fatal(err)
		}
		if _, err := x.Accept(raw); !errors.Is(err, ErrPeerMessage) {
			t.Fatalf("second terminal response: %v", err)
		}
	}
}

func checkBadMessages(t *testing.T, x *Exchange) {
	t.Helper()
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":"7","result":{"resultType":"complete","tools":[],"ttlMs":0,"cacheScope":"private"}}`,
		`{"jsonrpc":"2.0","id":7,"result":{"tools":[]}}`,
		`{"jsonrpc":"2.0","id":7,"result":{"resultType":"task","tools":[]}}`,
		`{"jsonrpc":"2.0","id":7,"result":{"resultType":"complete","tools":{},"ttlMs":0,"cacheScope":"private"}}`,
		`{"jsonrpc":"2.0","id":7,"result":{"resultType":"complete","tools":[]}}`,
		`{"jsonrpc":"2.0","id":7,"result":{"resultType":"complete","tools":[],"ttlMs":"0","cacheScope":"private"}}`,
		`{"jsonrpc":"2.0","id":7,"result":{"resultType":"complete","tools":[{"name":7}],"ttlMs":0,"cacheScope":"private"}}`,
		`{"jsonrpc":"2.0","id":7,"error":{"code":-32022,"message":"unsupported","data":{"requested":null,"supported":["2026-07-28"]}}}`,
		`{"jsonrpc":"2.0","id":7,"error":{"code":-32021,"message":"capability","data":{"requiredCapabilities":[]}}}`,
		`{"jsonrpc":"2.0","id":7,"error":{"code":-32002,"message":"resource not found"}}`,
		`{"jsonrpc":"2.0","id":7,"method":"sampling/createMessage"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/tasks/status","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":1,"progress":1}}`,
		`{"jsonrpc":"2.0","id":7,`,
	} {
		if _, err := x.Accept([]byte(raw)); !errors.Is(err, ErrPeerMessage) {
			t.Errorf("accepted %s (err=%v)", raw, err)
		}
	}
}

func TestSubscriptionSequence(t *testing.T) {
	request := fixtureRequest(t, `"sub"`, "subscriptions/listen", `,"notifications":{"toolsListChanged":true,"resourceSubscriptions":["a","b"]}`)
	x := NewExchange(request, MeshReply)
	ack := subNotification("subscriptions/acknowledged", `"sub"`, `,"notifications":{"resourceSubscriptions":["a"]}`)
	for _, raw := range [][]byte{
		subNotification("resources/updated", `"sub"`, `,"uri":"a"`),
		subNotification("subscriptions/acknowledged", "1", `,"notifications":{}`),
		subNotification("subscriptions/acknowledged", `"sub"`, `,"notifications":{"promptsListChanged":true}`),
	} {
		if _, err := x.Accept(raw); !errors.Is(err, ErrPeerMessage) {
			t.Errorf("accepted %s (err=%v)", raw, err)
		}
	}
	for _, raw := range [][]byte{ack, subNotification("resources/updated", `"sub"`, `,"uri":"a"`)} {
		if event, err := x.Accept(raw); err != nil || event.Terminal {
			t.Fatalf("valid subscription notification %s: %v", raw, err)
		}
	}
	for _, raw := range [][]byte{
		ack, subNotification("tools/list_changed", `"sub"`, ""),
		subNotification("resources/updated", `"sub"`, `,"uri":"b"`),
	} {
		if _, err := x.Accept(raw); !errors.Is(err, ErrPeerMessage) {
			t.Errorf("accepted %s (err=%v)", raw, err)
		}
	}
	if !errors.Is(x.Finish(), ErrInterrupted) {
		t.Fatal("subscription with no terminal frame was not interrupted")
	}
	reply, err := Complete(request, json.RawMessage(`{}`), json.RawMessage(fixtureInfo), 0)
	if err != nil {
		t.Fatal(err)
	}
	if event, err := x.Accept(reply); err != nil || !event.Terminal || x.Finish() != nil {
		t.Fatalf("subscription completion: %+v, %v", event, err)
	}
}

func subNotification(method, id, extra string) []byte {
	return fmt.Appendf(nil, `{"jsonrpc":"2.0","method":"notifications/%s","params":{"_meta":{%q:%s}%s}}`, method, SubscriptionID, id, extra)
}

func FuzzFraming(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","id":9007199254740993,"method":"server/discover"}` + "\n"))
	f.Add([]byte("[]\n{\n\x00"))
	f.Fuzz(checkStdioFrameLengths)
}

func checkStdioFrameLengths(t *testing.T, raw []byte) {
	r := NewStdioReader(bytes.NewReader(raw))
	for {
		line, err := r.Read()
		if err != nil {
			return
		}
		message, fault := Parse(line)
		if fault == nil && (!json.Valid(message.Raw()) || len(line) > MaxMessage) {
			t.Fatalf("invalid message accepted: %s", line)
		}
	}
}

func TestMRTRRejectsUnnegotiatedAndMalformedResults(t *testing.T) {
	const input = `{"resultType":"input_required","requestState":"opaque","inputRequests":{"ask":{"method":"elicitation/create","params":{"mode":"form","message":"Confirm","requestedSchema":{"type":"object","properties":{}}}}}}`
	const params = `,"name":"echo"`
	offered := mustParse(t, requestBytes("1", "tools/call", Version, `{"elicitation":{"form":{}}}`, params))
	missing := fixtureRequest(t, "1", "tools/call", params)
	unsupported := fixtureRequest(t, "1", "tools/list", "")
	for _, tc := range []struct {
		request Message
		result  string
		valid   bool
	}{
		{offered, input, true},
		{missing, input, false},
		{unsupported, input, false},
		{offered, `{"resultType":"input_required","requestState":"opaque"}`, true},
		{offered, `{"resultType":"input_required","requestState":"opaque","ttlMs":0}`, false},
		{offered, `{"resultType":"input_required","inputRequests":{}}`, false},
		{offered, `{"resultType":"input_required","inputRequests":{"ask":{"method":"tasks/list"}}}`, false},
		{offered, strings.Replace(input, `"form"`, "17", 1), false},
	} {
		_, err := NewExchange(tc.request, StdioReply).Accept(envResult("1", tc.result))
		if (err == nil) != tc.valid {
			t.Errorf("%s with %s: got %v", tc.request.method, tc.result, err)
		}
	}
	for _, continuation := range []string{
		`,"requestState":[]`, `,"inputResponses":{"answer":[]}`, `,"inputResponses":{"answer":{"action":"unknown"}}`,
	} {
		m := mustParse(t, requestBytes("1", "tools/call", Version, "{}", params+continuation))
		if fault := ValidateRequest(m, fixtureMethods); fault == nil || fault.Code != InvalidParams {
			t.Errorf("accepted malformed continuation %s: %v", continuation, fault)
		}
	}
}
