package mcphost

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shaul/mesh/internal/mcpwire"
)

// A listen whose acknowledgment backs no filter watches nothing, so it must
// not keep an on-demand instance alive.
func TestEmptyListenDoesNotHoldInstance(t *testing.T) {
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "repo", Idle: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := make(chan string, 4)
	// The fixture backs only toolsListChanged, so this acknowledgment is empty.
	listen := parseRequest(t, `{"jsonrpc":"2.0","id":"L","method":"subscriptions/listen","params":{"notifications":{"promptsListChanged":true},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	go func() {
		_ = f.sup.Forward(ctx, caller("a"), Target{Service: "repo"}, listen, func(raw []byte, _ bool) error { events <- string(raw); return nil })
	}()
	if ack := waitEvent(t, events); string(member([]byte(ack), "params", "notifications")) != `{}` {
		t.Fatalf("fixture acknowledgment: %s", ack)
	}
	eventually(t, func() bool { return f.sup.Services(caller("a"))[0].Instances[0].State == StateStopped })
}

func TestDiscoverMasksTasks(t *testing.T) {
	router, err := NewRouter()
	if err != nil {
		t.Fatal(err)
	}
	lines := newScriptLines()
	u := newStdioUpstream(t.Context(), lines, router)
	defer func() { _ = u.Close() }()
	route := Route{Caller: Caller{Account: "local", App: "a"}, Upstream: u}
	discover := parseRequest(t, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	out := &collected{}
	done := make(chan error, 1)
	go func() { done <- router.Forward(t.Context(), route, discover, out.emit) }()
	id := lines.lastID(t)
	lines.out <- []byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{},"extensions":{"io.modelcontextprotocol/tasks":{},"com.example/other":{}}},"ttlMs":0,"cacheScope":"private"}}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	extensions := member([]byte(out.last()), "result", "capabilities", "extensions")
	if member(extensions, tasksExtension) != nil || member(extensions, "com.example/other") == nil {
		t.Fatalf("hosted discover extensions: %s", extensions)
	}
}

// A compliant HTTP server without tools is ready without a tools/list.
func TestExternalServerWithoutTools(t *testing.T) {
	t.Setenv("MCPGODEBUG", "")
	server := mcp.NewServer(&mcp.Implementation{Name: "prompts-only", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{mcpwire.Version}})
	server.AddPrompt(&mcp.Prompt{Name: "hello"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "hi"}}}}, nil
	})
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	// A server without the tools capability may refuse tools/list outright.
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &call)
		if call.Method == mcpwire.MethodListTools {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(call.ID) + `,"error":{"code":-32601,"message":"no tools"}}`))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sdk.ServeHTTP(w, r)
	}))
	defer fixture.Close()
	f := newSupervisorFixture(t)
	f.publish(t, Definition{Name: "prompts", Argv: []string{}, URL: fixture.URL + "/mcp", InstancePolicy: PolicyShared})
	ctx, cancel := context.WithTimeout(t.Context(), testHang)
	defer cancel()
	out := &collected{}
	get := parseRequest(t, `{"jsonrpc":"2.0","id":"g","method":"prompts/get","params":{"name":"hello","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	if err := f.sup.Forward(ctx, caller("a"), Target{Service: "prompts"}, get, out.emit); err != nil {
		t.Fatalf("prompts-only server: %v", err)
	}
	var result struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(member([]byte(out.last()), "result"), &result); err != nil || len(result.Messages) != 1 || !strings.Contains(out.last(), "hi") {
		t.Fatalf("prompts/get: %s", out.last())
	}
}
