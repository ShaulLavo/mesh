package mcpconnect

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/protocol/mcpframe"
	"github.com/shaul/mesh/internal/transport"
)

const testMeta = `"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}`

func canonicalTestHostID() string {
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func TestInstanceHandleRoundTripAndValidation(t *testing.T) {
	hostID := canonicalTestHostID()
	svcID := strings.Repeat("a", 32)
	instID := strings.Repeat("b", 32)

	handle, err := FormatInstanceHandle(hostID, svcID, instID)
	if err != nil {
		t.Fatalf("FormatInstanceHandle: %v", err)
	}
	parsed, err := ParseInstanceHandle(handle)
	if err != nil {
		t.Fatalf("ParseInstanceHandle: %v", err)
	}
	if parsed.HostID != hostID || parsed.ServiceID != svcID || parsed.InstanceID != instID || parsed.String() != handle {
		t.Fatalf("parsed mismatch: %+v", parsed)
	}
	for _, bad := range []string{
		"",
		"mc1." + hostID + "." + svcID,
		"mi1.short." + svcID + "." + instID,
		"mi1." + hostID + ".BAD." + instID,
		"mi1." + hostID + "." + svcID + "." + strings.ToUpper(instID),
	} {
		if _, err := ParseInstanceHandle(bad); !errors.Is(err, ErrInvalidHandle) {
			t.Fatalf("ParseInstanceHandle(%q) = %v, want ErrInvalidHandle", bad, err)
		}
	}
}

func TestRewriteResultServerInfoPreservesUpstreamAndVendorFields(t *testing.T) {
	meshInfo := json.RawMessage(`{"name":"mesh","version":"1.2.3"}`)
	upstreamInfo := json.RawMessage(`{"name":"repo-mcp","version":"4.5.6"}`)

	// Discover reply carries upstream serverInfo in standard key -> moved to
	// upstreamServerInfo while Mesh sets standard serverInfo and keeps vendor fields.
	discover := []byte(`{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","supportedVersions":["2025-11-25","2026-07-28"],"capabilities":{"tools":{}},"ttlMs":60000,"cacheScope":"public","x-vendor-num":900719925474099312345,"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"repo-mcp","version":"4.5.6"},"com.example/trace":"abc"}}}`)
	rewritten, observed, err := rewriteForwardedResult(mcpwire.MethodDiscover, discover, meshInfo, nil)
	if err != nil {
		t.Fatalf("rewrite discover: %v", err)
	}
	if string(observed) != `{"name":"repo-mcp","version":"4.5.6"}` {
		t.Fatalf("observed = %s", observed)
	}
	if !strings.Contains(string(rewritten), `"x-vendor-num":900719925474099312345`) {
		t.Fatalf("vendor field lost: %s", rewritten)
	}
	if !strings.Contains(string(rewritten), `"com.example/trace":"abc"`) {
		t.Fatalf("vendor meta lost: %s", rewritten)
	}
	if !strings.Contains(string(rewritten), `"com.github.shaullavo.mesh/upstreamServerInfo":{"name":"repo-mcp","version":"4.5.6"}`) {
		t.Fatalf("upstreamServerInfo missing: %s", rewritten)
	}
	if !strings.Contains(string(rewritten), `"io.modelcontextprotocol/serverInfo":{"name":"mesh","version":"1.2.3"}`) {
		t.Fatalf("mesh serverInfo missing: %s", rewritten)
	}
	if !strings.Contains(string(rewritten), `"cacheScope":"private"`) || strings.Contains(string(rewritten), `"public"`) {
		t.Fatalf("discover cacheScope not forced to private: %s", rewritten)
	}
	if !strings.Contains(string(rewritten), `"supportedVersions":["2026-07-28"]`) || strings.Contains(string(rewritten), "2025-11-25") {
		t.Fatalf("discover supportedVersions not pinned to 2026-07-28: %s", rewritten)
	}

	// List reply with cacheScope=public is forced to private.
	listReply := []byte(`{"jsonrpc":"2.0","id":2,"result":{"resultType":"complete","tools":[],"ttlMs":60000,"cacheScope":"public"}}`)
	rewrittenList, _, err := rewriteForwardedResult(mcpwire.MethodListTools, listReply, meshInfo, upstreamInfo)
	if err != nil {
		t.Fatalf("rewrite list: %v", err)
	}
	if !strings.Contains(string(rewrittenList), `"cacheScope":"private"`) || strings.Contains(string(rewrittenList), `"public"`) {
		t.Fatalf("list cacheScope not forced to private: %s", rewrittenList)
	}

	// Continuation (input_required) strips ttlMs and cacheScope.
	contReply := []byte(`{"jsonrpc":"2.0","id":3,"result":{"resultType":"input_required","requestState":"opaque","ttlMs":60000,"cacheScope":"public","inputRequests":{}}}`)
	rewrittenCont, _, err := rewriteForwardedResult(mcpwire.MethodCallTool, contReply, meshInfo, upstreamInfo)
	if err != nil {
		t.Fatalf("rewrite continuation: %v", err)
	}
	if strings.Contains(string(rewrittenCont), `"ttlMs"`) || strings.Contains(string(rewrittenCont), `"cacheScope"`) {
		t.Fatalf("continuation retained cache hints: %s", rewrittenCont)
	}

	// Subsequent tool reply without upstream serverInfo falls back to observed.
	toolReply := []byte(`{"jsonrpc":"2.0","id":4,"result":{"resultType":"complete","content":[{"type":"text","text":"ok"}]}}`)
	rewritten2, _, err := rewriteForwardedResult(mcpwire.MethodCallTool, toolReply, meshInfo, upstreamInfo)
	if err != nil {
		t.Fatalf("rewrite tool reply: %v", err)
	}
	if !strings.Contains(string(rewritten2), `"com.github.shaullavo.mesh/upstreamServerInfo":{"name":"repo-mcp","version":"4.5.6"}`) {
		t.Fatalf("fallback upstreamServerInfo missing: %s", rewritten2)
	}
}

type pipeConn struct {
	rd         chan protocol.Frame
	wr         chan protocol.Frame
	closed     chan struct{}
	peerClosed chan struct{}
	once       sync.Once
}

func newPipePair() (*pipeConn, *pipeConn) {
	aToB := make(chan protocol.Frame, 32)
	bToA := make(chan protocol.Frame, 32)
	aClosed := make(chan struct{})
	bClosed := make(chan struct{})
	a := &pipeConn{rd: bToA, wr: aToB, closed: aClosed, peerClosed: bClosed}
	b := &pipeConn{rd: aToB, wr: bToA, closed: bClosed, peerClosed: aClosed}
	return a, b
}

func (c *pipeConn) ReadFrame() (protocol.Frame, error) {
	select {
	case f := <-c.rd:
		return f, nil
	case <-c.closed:
		return protocol.Frame{}, io.EOF
	case <-c.peerClosed:
		select {
		case f := <-c.rd:
			return f, nil
		default:
			return protocol.Frame{}, io.EOF
		}
	}
}

func (c *pipeConn) WriteFrame(f protocol.Frame) error {
	select {
	case <-c.closed:
		return io.ErrClosedPipe
	case <-c.peerClosed:
		return io.ErrClosedPipe
	case c.wr <- f:
		return nil
	}
}

func (c *pipeConn) Close() error {
	c.once.Do(func() {
		close(c.closed)
	})
	return nil
}

type fakeStream struct {
	conn      transport.Conn
	hostID    string
	supported bool
	onClose   func()
	once      sync.Once
}

func (s *fakeStream) Conn() transport.Conn      { return s.conn }
func (s *fakeStream) HostID() string            { return s.hostID }
func (s *fakeStream) MCPHostingSupported() bool { return s.supported }
func (s *fakeStream) Close() {
	s.once.Do(func() {
		_ = s.conn.Close()
		if s.onClose != nil {
			s.onClose()
		}
	})
}

func runServerSession(t *testing.T, opts ServerOptions, send func(in io.Writer, rd *bufio.Reader)) {
	t.Helper()
	srv, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(t.Context(), inR, outW)
		_ = outW.Close()
	}()
	send(inW, bufio.NewReader(outR))
	_ = inW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not exit after stdin EOF")
	}
}

func readJSONLine(t *testing.T, rd *bufio.Reader) map[string]any {
	t.Helper()
	line, err := rd.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read stdout line: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil {
		t.Fatalf("decode stdout JSON %q: %v", line, err)
	}
	return obj
}

func TestH3ForwardRoundTripProgressCancellationAndRetryRules(t *testing.T) {
	hostID := canonicalTestHostID()
	var discoverAttempts atomic.Int32
	var toolAttempts atomic.Int32
	cancelledOnServer := make(chan struct{}, 1)

	forwarder := &H3Forwarder{
		HostID: hostID,
		Route:  mcpframe.Route{Service: "repo-tools", Caller: protocol.CallerCredential("cred")},
		Open: func(_ context.Context, _ bool) (Stream, error) {
			clientConn, serverConn := newPipePair()
			go func() {
				defer func() { _ = serverConn.Close() }()
				frame, err := serverConn.ReadFrame()
				if err != nil {
					return
				}
				route, msg, err := mcpframe.DecodeForward(frame)
				if err != nil || route.Service != "repo-tools" {
					return
				}
				switch msg.Method() {
				case mcpwire.MethodDiscover:
					if discoverAttempts.Add(1) == 1 {
						// Drop first discover attempt to prove single read-only retry.
						return
					}
					raw := []byte(`{"jsonrpc":"2.0","id":` + string(mustID(msg)) + `,"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"ttlMs":60000,"cacheScope":"private","_meta":{"io.modelcontextprotocol/serverInfo":{"name":"upstream-srv","version":"9.0"}}}}`)
					f, _ := mcpframe.Encode(mcpframe.Result, raw)
					_ = serverConn.WriteFrame(f)
				case mcpwire.MethodCallTool:
					var p struct {
						Name string `json:"name"`
					}
					_ = json.Unmarshal(msg.Params(), &p)
					switch p.Name {
					case "slow":
						// Wait for client close (cancellation).
						_, _ = serverConn.ReadFrame()
						select {
						case cancelledOnServer <- struct{}{}:
						default:
						}
					case "drop":
						toolAttempts.Add(1)
						// Drop without terminal reply -> must NOT retry tool calls.
						return
					case "progress":
						p1, _ := mcpframe.Encode(mcpframe.Message, []byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"tok1","progress":1,"total":2}}`))
						_ = serverConn.WriteFrame(p1)
						res, _ := mcpframe.Encode(mcpframe.Result, []byte(`{"jsonrpc":"2.0","id":`+string(mustID(msg))+`,"result":{"resultType":"complete","content":[{"type":"text","text":"done"}]}}`))
						_ = serverConn.WriteFrame(res)
					}
				}
			}()
			return &fakeStream{conn: clientConn, hostID: hostID, supported: true}, nil
		},
	}

	runServerSession(t, ServerOptions{
		Version:   "0.9.9",
		HostID:    hostID,
		Service:   "repo-tools",
		Forwarder: forwarder,
	}, func(in io.Writer, rd *bufio.Reader) {
		// 1. Discover retries once on transport drop and preserves upstreamServerInfo.
		_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{`+testMeta+`}}}`+"\n")
		reply := readJSONLine(t, rd)
		if discoverAttempts.Load() != 2 {
			t.Fatalf("discover attempts = %d, want 2", discoverAttempts.Load())
		}
		meta := reply["result"].(map[string]any)["_meta"].(map[string]any)
		if meta["io.modelcontextprotocol/serverInfo"].(map[string]any)["name"] != "mesh" {
			t.Fatalf("unexpected serverInfo: %+v", meta)
		}
		if meta[MetaUpstreamServerInfo].(map[string]any)["name"] != "upstream-srv" {
			t.Fatalf("unexpected upstreamServerInfo: %+v", meta)
		}

		// 2. Progress notification followed by terminal result preserving observed upstreamServerInfo.
		_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":"p1","method":"tools/call","params":{"name":"progress","arguments":{},"_meta":{`+testMeta+`,"progressToken":"tok1"}}}`+"\n")
		var sawResult map[string]any
		for {
			msg := readJSONLine(t, rd)
			if msg["method"] == "notifications/progress" {
				continue
			}
			sawResult = msg
			break
		}
		if sawResult["id"] != "p1" {
			t.Fatalf("expected string id p1, got %+v", sawResult)
		}
		toolMeta := sawResult["result"].(map[string]any)["_meta"].(map[string]any)
		if toolMeta[MetaUpstreamServerInfo].(map[string]any)["name"] != "upstream-srv" {
			t.Fatalf("fallback upstreamServerInfo missing on tool call: %+v", toolMeta)
		}

		// 3. Dropped tool call returns PLAN §2.4 isError tool result with request.outcome_unknown and is NEVER retried.
		_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"drop","arguments":{},"_meta":{`+testMeta+`}}}`+"\n")
		dropReply := readJSONLine(t, rd)
		if toolAttempts.Load() != 1 {
			t.Fatalf("tool call was retried: attempts=%d", toolAttempts.Load())
		}
		if dropReply["error"] != nil {
			t.Fatalf("expected PLAN §2.4 tool-error result, got JSON-RPC error: %+v", dropReply)
		}
		dropRes := dropReply["result"].(map[string]any)
		if dropRes["isError"] != true {
			t.Fatalf("expected isError=true, got %+v", dropRes)
		}
		sc := dropRes["structuredContent"].(map[string]any)
		if sc["status"] != string(agentapi.StatusError) {
			t.Fatalf("expected agent envelope status=error, got %+v", sc)
		}
		errs := sc["errors"].([]any)
		firstErr := errs[0].(map[string]any)
		if firstErr["code"] != string(agentapi.RequestOutcomeUnknown) || firstErr["retryable"] != false {
			t.Fatalf("expected request.outcome_unknown retryable=false, got %+v", firstErr)
		}

		// 4. Slow call cancelled by notifications/cancelled closes the H3 stream.
		_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"slow","arguments":{},"_meta":{`+testMeta+`}}}`+"\n")
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(in, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":4}}`+"\n")
		select {
		case <-cancelledOnServer:
		case <-time.After(3 * time.Second):
			t.Fatal("H3 stream was not closed on notifications/cancelled")
		}

		// 5. Legacy initialize is refused with -32601.
		_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":5,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{}}}`+"\n")
		legacy := readJSONLine(t, rd)
		if int(legacy["error"].(map[string]any)["code"].(float64)) != mcpwire.MethodNotFound {
			t.Fatalf("expected -32601 for legacy initialize, got %+v", legacy)
		}
	})
}

func TestH3UnsupportedHostAndSeparateStreamBudget(t *testing.T) {
	hostID := canonicalTestHostID()
	unsupportedForwarder := &H3Forwarder{
		HostID: hostID,
		Route:  mcpframe.Route{Service: "svc"},
		Open: func(_ context.Context, _ bool) (Stream, error) {
			c, _ := newPipePair()
			return &fakeStream{conn: c, hostID: hostID, supported: false}, nil
		},
	}
	runServerSession(t, ServerOptions{
		HostID:    hostID,
		Service:   "svc",
		Forwarder: unsupportedForwarder,
	}, func(in io.Writer, rd *bufio.Reader) {
		_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{`+testMeta+`}}}`+"\n")
		reply := readJSONLine(t, rd)
		code := reply["error"].(map[string]any)["data"].(map[string]any)["meshCode"]
		if code != string(agentapi.ProtocolUnsupported) {
			t.Fatalf("expected protocol.unsupported, got %+v", reply)
		}
	})

	// Prove listen stream slots are independent of ordinary request slots.
	var listenHeld atomic.Int32
	unblock := make(chan struct{})
	streamForwarder := &H3Forwarder{
		HostID: hostID,
		Route:  mcpframe.Route{Service: "svc"},
		Open: func(_ context.Context, listen bool) (Stream, error) {
			c, s := newPipePair()
			go func() {
				defer func() { _ = s.Close() }()
				frame, err := s.ReadFrame()
				if err != nil {
					return
				}
				_, msg, err := mcpframe.DecodeForward(frame)
				if err != nil {
					return
				}
				if listen {
					listenHeld.Add(1)
					ack, _ := mcpframe.Encode(mcpframe.Message, []byte(`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"notifications":{"toolsListChanged":true},"_meta":{"io.modelcontextprotocol/subscriptionId":`+string(mustID(msg))+`}}}`))
					_ = s.WriteFrame(ack)
					<-unblock
					done, _ := mcpframe.Encode(mcpframe.Result, []byte(`{"jsonrpc":"2.0","id":`+string(mustID(msg))+`,"result":{"resultType":"complete","_meta":{"io.modelcontextprotocol/subscriptionId":`+string(mustID(msg))+`}}}`))
					_ = s.WriteFrame(done)
					return
				}
				res, _ := mcpframe.Encode(mcpframe.Result, []byte(`{"jsonrpc":"2.0","id":`+string(mustID(msg))+`,"result":{"resultType":"complete","content":[{"type":"text","text":"fast"}]}}`))
				_ = s.WriteFrame(res)
			}()
			return &fakeStream{conn: c, hostID: hostID, supported: true}, nil
		},
	}

	runServerSession(t, ServerOptions{
		HostID:      hostID,
		Service:     "svc",
		MaxStreams:  1,
		MaxRequests: 2,
		Forwarder:   streamForwarder,
	}, func(in io.Writer, rd *bufio.Reader) {
		// Occupy the single stream slot.
		_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":"sub1","method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},"_meta":{`+testMeta+`}}}`+"\n")
		ack := readJSONLine(t, rd)
		if ack["method"] != "notifications/subscriptions/acknowledged" {
			t.Fatalf("expected ack, got %+v", ack)
		}
		// Second listen is refused with resource.limit.
		_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":"sub2","method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},"_meta":{`+testMeta+`}}}`+"\n")
		refused := readJSONLine(t, rd)
		if refused["error"].(map[string]any)["data"].(map[string]any)["meshCode"] != string(agentapi.ResourceLimit) {
			t.Fatalf("expected resource.limit on 2nd listen, got %+v", refused)
		}
		// Ordinary tool call STILL succeeds because stream slots are separate!
		_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"fast","arguments":{},"_meta":{`+testMeta+`}}}`+"\n")
		fast := readJSONLine(t, rd)
		if fast["id"].(float64) != 10 || fast["error"] != nil {
			t.Fatalf("ordinary request blocked by listen: %+v", fast)
		}
		close(unblock)
		done := readJSONLine(t, rd)
		if done["id"] != "sub1" || done["error"] != nil {
			t.Fatalf("expected sub1 completion, got %+v", done)
		}
	})
}

func mustID(m mcpwire.Message) []byte {
	b, _ := m.ID().MarshalJSON()
	return b
}
