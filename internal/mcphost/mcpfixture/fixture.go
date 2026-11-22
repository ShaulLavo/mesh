// Package mcpfixture is a small stateless MCP 2026-07-28 stdio server for
// tests. Its tools exercise large replies, progress, cancellation,
// multi-round input, subscriptions and protocol faults.
package mcpfixture

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Env names that change the fixture's behaviour.
const (
	// EnvMode selects "legacy" (refuses discovery like a pre-2026 server),
	// "bad-after-probe" (answers discovery, then sends results without
	// resultType) or "exit" (exits before reading anything).
	EnvMode = "MESH_MCP_FIXTURE_MODE"
	// EnvSecret is printed to stderr at start, as a careless server would.
	EnvSecret = "MESH_MCP_FIXTURE_SECRET"
)

type server struct {
	out    *bufio.Writer
	stderr io.Writer
	mode   string
	mu     sync.Mutex

	pending  sync.Map // request ID string -> chan struct{} closed on cancel
	listens  sync.Map // subscription ID raw -> struct{}
	calls    int
	instance string
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type callParams struct {
	Name           string                     `json:"name"`
	Arguments      map[string]json.RawMessage `json:"arguments"`
	RequestState   *string                    `json:"requestState"`
	InputResponses map[string]json.RawMessage `json:"inputResponses"`
	Meta           map[string]json.RawMessage `json:"_meta"`
}

// Run serves MCP on stdin and stdout until stdin closes.
func Run(stdin io.Reader, stdout, stderr io.Writer) error {
	s := &server{out: bufio.NewWriterSize(stdout, 64<<10), stderr: stderr, mode: os.Getenv(EnvMode), instance: strconv.Itoa(os.Getpid())}
	if s.mode == "exit" {
		_, _ = fmt.Fprintln(stderr, "fixture: exiting at start")
		return fmt.Errorf("fixture exit mode")
	}
	if secret := os.Getenv(EnvSecret); secret != "" {
		_, _ = fmt.Fprintf(stderr, "fixture: using token %s\n", secret)
	}
	reader := bufio.NewReaderSize(stdin, 64<<10)
	// Like a real server, closing stdin ends the process without waiting for
	// requests still in progress.
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			go s.handle(line)
		}
		if err != nil {
			return nil
		}
	}
}

func (s *server) write(value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	s.writeRaw(raw)
}

func (s *server) writeRaw(raw []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(raw)
	_ = s.out.WriteByte('\n')
	_ = s.out.Flush()
}

func (s *server) result(id json.RawMessage, result map[string]any) {
	if _, ok := result["resultType"]; !ok && s.mode != "bad-after-probe" {
		result["resultType"] = "complete"
	}
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *server) fail(id json.RawMessage, code int, message string) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func (s *server) handle(line []byte) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}
	if req.ID == nil {
		s.notification(req)
		return
	}
	switch req.Method {
	case "server/discover":
		s.discover(req)
	case "tools/list":
		s.result(req.ID, map[string]any{"tools": tools(), "ttlMs": 60000, "cacheScope": "private"})
	case "tools/call":
		s.call(req)
	case "subscriptions/listen":
		s.listen(req)
	default:
		s.fail(req.ID, -32601, "method not found")
	}
}

func (s *server) notification(req request) {
	if req.Method != "notifications/cancelled" {
		return
	}
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	_ = json.Unmarshal(req.Params, &params)
	_, _ = fmt.Fprintf(s.stderr, "fixture: cancelled %s\n", params.RequestID)
	if done, ok := s.pending.LoadAndDelete(string(params.RequestID)); ok {
		close(done.(chan struct{}))
	}
	if _, ok := s.listens.LoadAndDelete(string(params.RequestID)); ok {
		_, _ = fmt.Fprintf(s.stderr, "fixture: listen ended %s\n", params.RequestID)
	}
}

func (s *server) discover(req request) {
	if s.mode == "legacy" {
		s.fail(req.ID, -32601, "method not found")
		return
	}
	s.write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
		"resultType":        "complete",
		"supportedVersions": []string{"2026-07-28"},
		"capabilities":      map[string]any{"tools": map[string]any{"listChanged": true}},
		"ttlMs":             60000,
		"cacheScope":        "private",
		"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "mesh-fixture", "version": "1"}},
	}})
}

func tools() []map[string]any {
	names := []string{"echo", "big", "slow", "progress", "ask", "whoami", "garbage", "touch", "exit"}
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}})
	}
	return out
}

func text(value string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": value}}}
}

func (s *server) call(req request) {
	var p callParams
	_ = json.Unmarshal(req.Params, &p)
	s.mu.Lock()
	s.calls++
	calls := s.calls
	s.mu.Unlock()
	switch p.Name {
	case "echo":
		var value string
		_ = json.Unmarshal(p.Arguments["text"], &value)
		s.result(req.ID, text(value))
	case "big":
		n, _ := strconv.Atoi(string(p.Arguments["bytes"]))
		s.result(req.ID, text(strings.Repeat("x", n)))
	case "slow":
		s.slow(req)
	case "progress":
		s.progress(req, p)
	case "ask":
		s.ask(req, p)
	case "whoami":
		sum := sha256.Sum256([]byte(os.Getenv(EnvSecret)))
		s.result(req.ID, text(fmt.Sprintf("instance=%s calls=%d secret-digest=%s", s.instance, calls, hex.EncodeToString(sum[:4]))))
	case "garbage":
		s.writeRaw([]byte("this is not json"))
	case "touch":
		s.listens.Range(func(key, _ any) bool {
			s.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed", "params": map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/subscriptionId": json.RawMessage(key.(string))}}})
			return true
		})
		s.result(req.ID, text("touched"))
	case "exit":
		s.result(req.ID, text("exiting"))
		time.Sleep(50 * time.Millisecond)
		os.Exit(3)
	default:
		s.fail(req.ID, -32602, "unknown tool")
	}
}

func (s *server) slow(req request) {
	done := make(chan struct{})
	s.pending.Store(string(req.ID), done)
	select {
	case <-done:
	case <-time.After(time.Minute):
		s.result(req.ID, text("slow finished"))
	}
}

func (s *server) progress(req request, p callParams) {
	n, _ := strconv.Atoi(string(p.Arguments["count"]))
	token := p.Meta["progressToken"]
	for i := range n {
		if token != nil {
			s.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"progressToken": token, "progress": i + 1, "total": n}})
		}
	}
	s.result(req.ID, text(fmt.Sprintf("progressed %d", n)))
}

func (s *server) ask(req request, p callParams) {
	if p.InputResponses == nil {
		s.write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
			"resultType":    "input_required",
			"requestState":  "fixture-state-1",
			"inputRequests": map[string]any{"name": map[string]any{"method": "elicitation/create", "params": map[string]any{"message": "Name?", "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}}}},
		}})
		return
	}
	if p.RequestState == nil || *p.RequestState != "fixture-state-1" {
		s.result(req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": "bad state"}}, "isError": true})
		return
	}
	var answer struct {
		Content struct {
			Name string `json:"name"`
		} `json:"content"`
	}
	_ = json.Unmarshal(p.InputResponses["name"], &answer)
	s.result(req.ID, text("hello "+answer.Content.Name))
}

func (s *server) listen(req request) {
	var p struct {
		Notifications map[string]json.RawMessage `json:"notifications"`
	}
	_ = json.Unmarshal(req.Params, &p)
	acknowledged := map[string]any{}
	if string(p.Notifications["toolsListChanged"]) == "true" {
		acknowledged["toolsListChanged"] = true
	}
	s.listens.Store(string(req.ID), struct{}{})
	s.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/subscriptions/acknowledged", "params": map[string]any{"notifications": acknowledged, "_meta": map[string]any{"io.modelcontextprotocol/subscriptionId": req.ID}}})
}
