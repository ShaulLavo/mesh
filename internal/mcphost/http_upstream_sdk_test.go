package mcphost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shaul/mesh/internal/mcpwire"
)

const regionalSchema = `{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}`

var sdkMethods = mcpwire.Methods{
	"server/discover": nil, "tools/list": nil, "tools/call": nil, "prompts/list": nil,
	"prompts/get": nil, "resources/list": nil, "resources/templates/list": nil,
	"resources/read": nil, "subscriptions/listen": nil, "completion/complete": nil,
}

// newSDKServer is the independent positive peer: the official Go SDK in its
// stateless Streamable HTTP mode.
func newSDKServer(stopped chan<- error) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "reference", Version: "1.8.0"},
		&mcp.ServerOptions{SupportedProtocolVersions: []string{mcpwire.Version}})
	server.AddTool(&mcp.Tool{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if len(req.Params.InputResponses) == 0 && !bytes.Contains(req.Params.Arguments, []byte(`"plain"`)) {
				return &mcp.CallToolResult{RequestState: "opaque", InputRequests: mcp.InputRequestMap{
					"confirm": &mcp.ElicitParams{Message: "Confirm fixture", RequestedSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)},
				}}, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	server.AddTool(&mcp.Tool{Name: "regional", InputSchema: json.RawMessage(regionalSchema)},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}}}, nil
		})
	server.AddTool(&mcp.Tool{Name: "wait", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: "track", Progress: 1}); err != nil {
				stopped <- err
				return nil, fmt.Errorf("send SDK fixture progress: %w", err)
			}
			<-ctx.Done()
			stopped <- ctx.Err()
			return nil, ctx.Err()
		})
	return server
}

// startSDK serves the SDK behind the same validation H6 applies: endpoint,
// credential and mirrored parameter headers checked against the server's own schema.
func startSDK(t *testing.T, jsonResponse bool, stopped chan<- error) string {
	t.Helper()
	t.Setenv("MCPGODEBUG", "")
	serverSchemas, err := CompileToolHeaders(json.RawMessage(`{"tools":[{"name":"regional","inputSchema":` + regionalSchema + `}]}`))
	if err != nil {
		t.Fatal(err)
	}
	policy := mcpwire.HTTPPolicy{Methods: sdkMethods, ToolHeaders: func(name string) mcpwire.ParameterHeaders {
		headers, _ := serverSchemas.Lookup(name)
		return headers
	}}
	server := newSDKServer(stopped)
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: jsonResponse, PropagateRequestCancellation: true,
	})
	fixture := httptest.NewUnstartedServer(nil)
	authority := fixture.Listener.Addr().String()
	fixture.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status := mcpwire.EndpointStatus(r, authority, "/mcp", nil); status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		input, err := mcpwire.ReadHTTP(r, policy)
		if err != nil || input.Status != http.StatusOK {
			writeInputFault(w, input, err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(input.Message.Raw()))
		sdk.ServeHTTP(w, r)
	})
	fixture.Start()
	t.Cleanup(fixture.Close)
	return fixture.URL + "/mcp"
}

func writeInputFault(w http.ResponseWriter, input mcpwire.HTTPInput, err error) {
	if err != nil || input.Fault == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	raw, _ := input.Fault.Reply()
	writeJSON(w, input.Status, string(raw))
}

func resultType(t *testing.T, event mcpwire.Event) string {
	t.Helper()
	var result struct {
		ResultType string `json:"resultType"`
	}
	if err := json.Unmarshal(event.Message.Result(), &result); err != nil {
		t.Fatalf("result %s: %v", event.Message.Raw(), err)
	}
	return result.ResultType
}

func TestSDKStatelessPeer(t *testing.T) {
	for name, jsonResponse := range map[string]bool{"JSON": true, "SSE": false} {
		t.Run(name, func(t *testing.T) { testSDKConversation(t, jsonResponse) })
	}
}

func testSDKConversation(t *testing.T, jsonResponse bool) {
	t.Helper()
	endpoint := startSDK(t, jsonResponse, make(chan error, 4))
	var schemas atomic.Pointer[ToolHeaders]
	upstream := dialUpstream(t, endpoint, bearer(), func(name string) (mcpwire.ParameterHeaders, bool) {
		known := schemas.Load()
		if known == nil {
			return mcpwire.ParameterHeaders{}, false
		}
		return known.Lookup(name)
	})
	step := func(m mcpwire.Message) mcpwire.Event {
		t.Helper()
		events, err := callAll(t, upstream, m)
		if err != nil || len(events) != 1 || !events[0].Terminal {
			t.Fatalf("%s: %d events, %v", m.Method(), len(events), err)
		}
		return events[0]
	}
	if got := resultType(t, step(request(t, "1", mcpwire.MethodDiscover, "", ""))); got != "complete" {
		t.Fatalf("discover: %s", got)
	}
	listed := step(request(t, "2", mcpwire.MethodListTools, "", ""))
	compiled, err := CompileToolHeaders(listed.Message.Result())
	if err != nil || len(compiled.Rejected) != 0 {
		t.Fatalf("SDK tool list: %v %v", err, compiled.Rejected)
	}
	if _, ok := compiled.Lookup("regional"); !ok {
		t.Fatalf("regional tool missing: %s", listed.Message.Raw())
	}
	if got := resultType(t, step(toolCall(t, "3", "echo", `{"plain":true}`, ""))); got != "complete" {
		t.Fatalf("plain call: %s", got)
	}

	// A call whose schema is not known yet is refused by the peer's header
	// check; the router refreshes once and retries with the new schema.
	regional := toolCall(t, "4", "regional", `{"region":"us-east-é"}`, "")
	_, err = callAll(t, upstream, regional)
	if !errors.Is(err, ErrHeaderMismatch) {
		t.Fatalf("stale schema: %v", err)
	}
	schemas.Store(&compiled)
	var delivered []mcpwire.Event
	err = upstream.Call(WithSchemaRefreshed(testContext(t)), regional, func(event mcpwire.Event) error {
		delivered = append(delivered, event)
		return nil
	})
	if err != nil || len(delivered) != 1 || resultType(t, delivered[0]) != "complete" {
		t.Fatalf("retry: %v %d", err, len(delivered))
	}
}

func TestSDKInputRequiredRoundTrip(t *testing.T) {
	endpoint := startSDK(t, true, make(chan error, 1))
	upstream := dialUpstream(t, endpoint, bearer(), nil)
	elicit := `"` + mcpwire.ClientCapsKey + `":{"elicitation":{"form":{}}}`
	first, err := callAll(t, upstream, rawRequest(t, `4`, `"echo"`, elicit, ""))
	if err != nil || len(first) != 1 || resultType(t, first[0]) != "input_required" {
		t.Fatalf("first round: %v %d", err, len(first))
	}
	second, err := callAll(t, upstream, rawRequest(t, `5`, `"echo"`, elicit,
		`,"requestState":"opaque","inputResponses":{"confirm":{"action":"accept","content":{"ok":true}}}`))
	if err != nil || len(second) != 1 || resultType(t, second[0]) != "complete" {
		t.Fatalf("second round: %v %d", err, len(second))
	}
}

// rawRequest builds a tools/call whose client capabilities differ from the default.
func rawRequest(t *testing.T, id, name, caps, extra string) mcpwire.Message {
	t.Helper()
	raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"_meta":{%q:%q,%s},"name":%s%s}}`,
		id, mcpwire.VersionKey, mcpwire.Version, caps, name, extra)
	m, fault := mcpwire.Parse([]byte(raw))
	if fault != nil {
		t.Fatal(fault)
	}
	return m
}

func TestSDKCancellationStopsTheHandler(t *testing.T) {
	stopped := make(chan error, 2)
	endpoint := startSDK(t, false, stopped)
	upstream := dialUpstream(t, endpoint, bearer(), nil)
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	err := upstream.Call(ctx, toolCall(t, "1", "wait", `{}`, `"progressToken":"track"`), func(event mcpwire.Event) error {
		if event.Message.Method() == "notifications/progress" {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if got := <-stopped; !errors.Is(got, context.Canceled) {
		t.Fatalf("SDK handler stopped with %v", got)
	}
}
