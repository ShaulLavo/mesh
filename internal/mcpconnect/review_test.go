package mcpconnect

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/protocol/mcpframe"
	"github.com/shaul/mesh/internal/transport"
)

type faultConn struct {
	writeErr  error
	readErr   error
	readQueue []protocol.Frame
	onWrite   func(protocol.Frame)
}

func (c *faultConn) WriteFrame(f protocol.Frame) error {
	if c.onWrite != nil {
		c.onWrite(f)
	}
	return c.writeErr
}

func (c *faultConn) ReadFrame() (protocol.Frame, error) {
	if len(c.readQueue) > 0 {
		f := c.readQueue[0]
		c.readQueue = c.readQueue[1:]
		return f, nil
	}
	if c.readErr != nil {
		return protocol.Frame{}, c.readErr
	}
	return protocol.Frame{}, io.EOF
}

func (c *faultConn) Close() error { return nil }

func mustParseReq(t *testing.T, raw string) mcpwire.Message {
	t.Helper()
	msg, fault := mcpwire.Parse([]byte(raw))
	if fault != nil {
		t.Fatalf("parse %s: %v", raw, fault)
	}
	return msg
}

func TestH3UncertainWriteAndNonRetryableProtocolErrors(t *testing.T) {
	hostID := canonicalTestHostID()

	// 1. Interrupted write on tools/call, prompts/get, resources/read, and continuation
	//    must report request.outcome_unknown (retryable=false) and NEVER retry.
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"tools/call", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy","arguments":{},"_meta":{` + testMeta + `}}}`},
		{"prompts/get", `{"jsonrpc":"2.0","id":2,"method":"prompts/get","params":{"name":"p","arguments":{},"_meta":{` + testMeta + `}}}`},
		{"resources/read", `{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"file:///a","_meta":{` + testMeta + `}}}`},
		{"continuation_list", `{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{"requestState":"ref-1","_meta":{` + testMeta + `}}}`},
	} {
		t.Run("write_error_"+tc.name, func(t *testing.T) {
			var opens atomic.Int32
			var hostSawFrame atomic.Bool
			fwd := &H3Forwarder{
				HostID: hostID,
				Route:  mcpframe.Route{Service: "svc"},
				Open: func(_ context.Context, _ bool) (Stream, error) {
					opens.Add(1)
					conn := &faultConn{
						writeErr: io.ErrUnexpectedEOF,
						onWrite:  func(protocol.Frame) { hostSawFrame.Store(true) },
					}
					return &fakeStream{conn: conn, hostID: hostID, supported: true}, nil
				},
			}
			err := fwd.Forward(t.Context(), mustParseReq(t, tc.raw), nil, func([]byte, bool) error { return nil })
			var rf *RouteFailure
			if !errors.As(err, &rf) {
				t.Fatalf("expected RouteFailure, got %v", err)
			}
			if !hostSawFrame.Load() || opens.Load() != 1 {
				t.Fatalf("hostSawFrame=%v opens=%d, want true and 1", hostSawFrame.Load(), opens.Load())
			}
			if rf.Code != agentapi.RequestOutcomeUnknown || rf.Retryable {
				t.Fatalf("got code=%s retryable=%v, want request.outcome_unknown retryable=false", rf.Code, rf.Retryable)
			}
		})
	}

	// 2. Proven pre-dispatch Open failure on tools/call remains host.unreachable (not outcome_unknown).
	t.Run("predispatch_open_error", func(t *testing.T) {
		var opens atomic.Int32
		fwd := &H3Forwarder{
			HostID: hostID,
			Route:  mcpframe.Route{Service: "svc"},
			Open: func(_ context.Context, _ bool) (Stream, error) {
				opens.Add(1)
				return nil, errors.New("dial tcp: connection refused")
			},
		}
		err := fwd.Forward(t.Context(), mustParseReq(t, `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"deploy","arguments":{},"_meta":{`+testMeta+`}}}`), nil, func([]byte, bool) error { return nil })
		var rf *RouteFailure
		if !errors.As(err, &rf) || rf.Code != agentapi.HostUnreachable || opens.Load() != 1 {
			t.Fatalf("pre-dispatch open failure = %v (opens=%d), want host.unreachable opens=1", err, opens.Load())
		}
	})

	// 3. Oversized reply frame and malformed frame on retryable tools/list are NEVER retried.
	for _, tc := range []struct {
		name     string
		readErr  error
		wantCode agentapi.Code
	}{
		{"oversized_frame", protocol.ErrPayloadTooLarge, agentapi.MCPMessageTooLarge},
		{"malformed_frame", transport.ErrInvalidFrame, agentapi.ServiceFailed},
	} {
		t.Run("read_"+tc.name, func(t *testing.T) {
			var opens atomic.Int32
			fwd := &H3Forwarder{
				HostID: hostID,
				Route:  mcpframe.Route{Service: "svc"},
				Open: func(_ context.Context, _ bool) (Stream, error) {
					opens.Add(1)
					return &fakeStream{conn: &faultConn{readErr: tc.readErr}, hostID: hostID, supported: true}, nil
				},
			}
			err := fwd.Forward(t.Context(), mustParseReq(t, `{"jsonrpc":"2.0","id":20,"method":"tools/list","params":{"_meta":{`+testMeta+`}}}`), nil, func([]byte, bool) error { return nil })
			var rf *RouteFailure
			if !errors.As(err, &rf) || rf.Code != tc.wantCode || rf.Retryable || opens.Load() != 1 {
				t.Fatalf("got err=%v opens=%d, want code=%s retryable=false opens=1", err, opens.Load(), tc.wantCode)
			}
		})
	}

	// 4. Failure after an emitted event on retryable tools/list is NEVER retried.
	t.Run("failure_after_emitted_event", func(t *testing.T) {
		var opens atomic.Int32
		prog, err := mcpframe.Encode(mcpframe.Message, []byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"t1","progress":1}}`))
		if err != nil {
			t.Fatal(err)
		}
		fwd := &H3Forwarder{
			HostID: hostID,
			Route:  mcpframe.Route{Service: "svc"},
			Open: func(_ context.Context, _ bool) (Stream, error) {
				opens.Add(1)
				return &fakeStream{conn: &faultConn{readQueue: []protocol.Frame{prog}, readErr: io.EOF}, hostID: hostID, supported: true}, nil
			},
		}
		emitted := 0
		err = fwd.Forward(t.Context(), mustParseReq(t, `{"jsonrpc":"2.0","id":30,"method":"tools/list","params":{"_meta":{`+testMeta+`,"progressToken":"t1"}}}`), nil, func([]byte, bool) error {
			emitted++
			return nil
		})
		var rf *RouteFailure
		if !errors.As(err, &rf) || rf.Code != agentapi.RequestOutcomeUnknown || opens.Load() != 1 || emitted != 1 {
			t.Fatalf("after-emit err=%v opens=%d emitted=%d, want request.outcome_unknown opens=1 emitted=1", err, opens.Load(), emitted)
		}
	})
}

func TestMeshErrorTranslationOnToolsVsNonToolsAndUpstreamErrors(t *testing.T) {
	hostID := canonicalTestHostID()
	fwd := &H3Forwarder{
		HostID: hostID,
		Route:  mcpframe.Route{Service: "svc"},
		Open: func(_ context.Context, _ bool) (Stream, error) {
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
				switch msg.Method() {
				case mcpwire.MethodCallTool:
					if strings.Contains(string(msg.Params()), `"host_mesh_err"`) {
						// Inherited host Mesh -31000 reply -> must become isError: true agent envelope on tools/call.
						raw := []byte(`{"jsonrpc":"2.0","id":` + string(mustID(msg)) + `,"error":{"code":-31000,"message":"service.unavailable: instance restarting","data":{"meshCode":"service.unavailable","retryable":true,"host":"` + hostID + `","service":"svc"}}}`)
						f, _ := mcpframe.Encode(mcpframe.Result, raw)
						_ = s.WriteFrame(f)
						return
					}
					// Upstream MCP JSON-RPC error (-32602) -> must be preserved verbatim!
					raw := []byte(`{"jsonrpc":"2.0","id":` + string(mustID(msg)) + `,"error":{"code":-32602,"message":"bad tool param"}}`)
					f, _ := mcpframe.Encode(mcpframe.Result, raw)
					_ = s.WriteFrame(f)
				case mcpwire.MethodReadResource:
					// Mesh failure outside tools/call -> keeps JSON-RPC -31000 with data.
					raw := []byte(`{"jsonrpc":"2.0","id":` + string(mustID(msg)) + `,"error":{"code":-31000,"message":"service.unavailable: down","data":{"meshCode":"service.unavailable","retryable":true,"host":"` + hostID + `","service":"svc"}}}`)
					f, _ := mcpframe.Encode(mcpframe.Result, raw)
					_ = s.WriteFrame(f)
				}
			}()
			return &fakeStream{conn: c, hostID: hostID, supported: true}, nil
		},
	}

	runServerSession(t, ServerOptions{HostID: hostID, Service: "svc", Forwarder: fwd}, func(in io.Writer, rd *bufio.Reader) {
		// 1. Host Mesh -31000 error on tools/call -> isError: true + agentapi.Envelope.
		_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"host_mesh_err","arguments":{},"_meta":{`+testMeta+`}}}`+"\n")
		rep1 := readJSONLine(t, rd)
		if rep1["error"] != nil {
			t.Fatalf("expected isError tool result, got JSON-RPC error: %+v", rep1)
		}
		res1 := rep1["result"].(map[string]any)
		sc := res1["structuredContent"].(map[string]any)
		errItem := sc["errors"].([]any)[0].(map[string]any)
		if res1["isError"] != true || errItem["code"] != string(agentapi.ServiceUnavailable) || errItem["retryable"] != true {
			t.Fatalf("unexpected tool error result: %+v", res1)
		}

		// 2. Upstream -32602 error on tools/call -> preserved verbatim.
		_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"bad","arguments":{},"_meta":{`+testMeta+`}}}`+"\n")
		rep2 := readJSONLine(t, rd)
		if rep2["error"] == nil || int(rep2["error"].(map[string]any)["code"].(float64)) != mcpwire.InvalidParams {
			t.Fatalf("upstream -32602 was not preserved: %+v", rep2)
		}

		// 3. Mesh -31000 error outside tools/call (resources/read) -> stays -31000.
		_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"file:///x","_meta":{`+testMeta+`}}}`+"\n")
		rep3 := readJSONLine(t, rd)
		if rep3["error"] == nil || int(rep3["error"].(map[string]any)["code"].(float64)) != -31000 {
			t.Fatalf("non-tool Mesh error did not stay -31000: %+v", rep3)
		}
	})
}

func TestH3BufferCountsAgainstFrontendBudget(t *testing.T) {
	hostID := canonicalTestHostID()
	bigPayload := strings.Repeat("a", 6000)
	fwd := &H3Forwarder{
		HostID: hostID,
		Route:  mcpframe.Route{Service: "svc"},
		Open: func(_ context.Context, _ bool) (Stream, error) {
			return &fakeStream{conn: &faultConn{}, hostID: hostID, supported: true}, nil
		},
	}
	// Create a tight 5 KiB budget -> encoded 6 KiB H3 request frame must be refused with resource.limit.
	var budget fakeBudget
	budget.limit = 5000
	err := fwd.Forward(t.Context(), mustParseReq(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"`+bigPayload+`"},"_meta":{`+testMeta+`}}}`), &budget, func([]byte, bool) error { return nil })
	var rf *RouteFailure
	if !errors.As(err, &rf) || rf.Code != agentapi.ResourceLimit {
		t.Fatalf("expected resource.limit when H3 request frame exceeds frontend byte budget, got %v", err)
	}
	if budget.used.Load() != 0 {
		t.Fatalf("budget leaked bytes: %d", budget.used.Load())
	}
}

type fakeBudget struct {
	limit int64
	used  atomic.Int64
}

func (b *fakeBudget) ReserveBytes(n int) bool {
	if b.used.Add(int64(n)) > b.limit {
		b.used.Add(-int64(n))
		return false
	}
	return true
}

func (b *fakeBudget) ReleaseBytes(n int) {
	b.used.Add(-int64(n))
}
