package mcphost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shaul/mesh/internal/mcpwire"
)

// sdkLines runs the official Go SDK v1.8.0 server as an independent
// positive peer behind a StdioUpstream.
type sdkLines struct {
	out  *bufio.Reader
	in   *io.PipeWriter
	mu   sync.Mutex
	stop func()
}

func startSDKLines(t *testing.T, server *mcp.Server) *sdkLines {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, &mcp.IOTransport{Reader: serverIn, Writer: serverOut}) }()
	l := &sdkLines{out: bufio.NewReader(clientIn), in: clientOut}
	l.stop = func() {
		cancel()
		for _, pipe := range []io.Closer{clientOut, clientIn, serverOut, serverIn} {
			_ = pipe.Close()
		}
		select {
		case <-done:
		case <-time.After(testHang):
			t.Error("SDK server did not stop")
		}
	}
	t.Cleanup(l.stop)
	return l
}

func (l *sdkLines) WriteLine(_ context.Context, line []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.in.Write(line); err != nil {
		return fmt.Errorf("SDK stdin: %w", err)
	}
	return nil
}

func (l *sdkLines) ReadLine() ([]byte, error) {
	line, err := l.out.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("SDK stdout: %w", err)
	}
	return bytes.TrimSuffix(line, []byte("\n")), nil
}

func (l *sdkLines) Close() error { return nil }

func newSDKPeer(t *testing.T) (*mcp.Server, <-chan error) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "sdk-peer", Version: "1.8.0"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{mcpwire.Version}})
	server.AddTool(&mcp.Tool{Name: "confirm", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if len(req.Params.InputResponses) == 0 {
				return &mcp.CallToolResult{RequestState: "sdk-state", InputRequests: mcp.InputRequestMap{
					"ok": &mcp.ElicitParams{Message: "Proceed?", RequestedSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)},
				}}, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "confirmed " + req.Params.RequestState}}}, nil
		})
	stopped := make(chan error, 1)
	server.AddTool(&mcp.Tool{Name: "wait", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			p := &mcp.ProgressNotificationParams{ProgressToken: req.Params.GetProgressToken(), Progress: 1}
			if err := req.Session.NotifyProgress(ctx, p); err != nil {
				return nil, fmt.Errorf("SDK peer progress: %w", err)
			}
			<-ctx.Done()
			stopped <- ctx.Err()
			return nil, fmt.Errorf("SDK peer wait: %w", ctx.Err())
		})
	return server, stopped
}

func TestSDKServerThroughTheRouter(t *testing.T) {
	t.Setenv("MCPGODEBUG", "")
	server, stopped := newSDKPeer(t)
	router, err := NewRouter()
	if err != nil {
		t.Fatal(err)
	}
	upstream := newStdioUpstream(t.Context(), startSDKLines(t, server), router.ids)
	defer func() { _ = upstream.Close() }()
	sup := &Supervisor{router: router}
	info, err := sup.probe(t.Context(), upstream, testHang)
	if err != nil || !strings.Contains(string(info), "sdk-peer") {
		t.Fatalf("probe of the SDK server: %s %v", info, err)
	}
	route := Route{Caller: Caller{Account: "local", App: "a", Grant: "g"}, Service: "sdk", Revision: 1, Instance: "i", Generation: "1", Upstream: upstream}
	h := &testHost{router: router, upstream: upstream}

	list, err := forward(t, h, route, parseRequest(t, `{"jsonrpc":"2.0","id":"list","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`))
	if err != nil || !strings.Contains(list.last(), `"confirm"`) || string(member([]byte(list.last()), "id")) != `"list"` {
		t.Fatalf("tools/list: %s %v", list.last(), err)
	}

	first, err := forward(t, h, route, callRequest(t, `11`, toolParams("confirm", `{}`, "")))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := decodeJSONString(member([]byte(first.last()), "result", "requestState"))
	if !strings.HasPrefix(ref, continuationPrefix) {
		t.Fatalf("SDK state reached the caller: %s", first.last())
	}
	retry := strings.TrimSuffix(toolParams("confirm", `{}`, ""), "}") + `,"requestState":"` + ref + `","inputResponses":{"ok":{"action":"accept","content":{"ok":true}}}}`
	second, err := forward(t, h, route, callRequest(t, `12`, retry))
	if err != nil || !strings.Contains(second.last(), "confirmed sdk-state") {
		t.Fatalf("continuation through the router: %s %v", second.last(), err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	progress := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- router.Forward(ctx, route, callRequest(t, `"w"`, toolParams("wait", `{}`, `"progressToken":"caller-token"`)), func(raw []byte, _ bool) error {
			progress <- string(raw)
			return nil
		})
	}()
	if got := waitEvent(t, progress); string(member(member([]byte(got), "params"), "progressToken")) != `"caller-token"` {
		t.Fatalf("SDK progress was not restored: %s", got)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled forward: %v", err)
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SDK handler stopped with %v", err)
		}
	case <-time.After(testHang):
		t.Fatal("the exact upstream cancellation did not stop the SDK handler")
	}
}
