package mcpnative

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/mcpwire"
)

// TestStdioEngineCancelledProgressAndOwnedNotificationsBounded verifies Item 7:
//  1. 1,000 sequential requests that emit progress and are cancelled while stdout
//     is paused retain zero progressList / progressElem entries.
//  2. Non-progress notifications queued by a request that is cancelled before
//     stdout drains are removed and never emitted.
func TestStdioEngineCancelledProgressAndOwnedNotificationsBounded(t *testing.T) {
	gated := newGatedWriter()
	blockerEntered := make(chan struct{})
	blockerDone := make(chan struct{})
	var notifySent sync.WaitGroup

	var engine *StdioEngine
	var err error
	engine, err = NewStdioEngine(StdioEngineOptions{
		Methods: mcpwire.Methods{
			mcpwire.MethodCallTool: {},
			mcpwire.MethodListen:   {},
		},
		IsStreamMethod:   func(m string) bool { return m == mcpwire.MethodListen },
		MaxFrontendBytes: 4 << 20,
		MaxConnBytes:     4 << 20,
		EOFDrainTimeout:  500 * time.Millisecond,
		Handler: func(ctx context.Context, msg mcpwire.Message, responder *StdioResponder) {
			if msg.Method() == mcpwire.MethodListen {
				// Queue a non-progress subscription notification, then wait for cancel.
				raw := []byte(`{"jsonrpc":"2.0","method":"notifications/subscriptions/event","params":{"event":"must-be-dropped"}}`)
				_ = responder.EnqueueNotification(raw)
				notifySent.Done()
				<-ctx.Done()
				responder.Complete(nil, nil)
				return
			}
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(msg.Params(), &p)
			if p.Name == "hold_writer" {
				// Emit a terminal reply so writerLoop enters gated.Write and blocks.
				reply, _ := mcpwire.Complete(msg, json.RawMessage(`{"content":[{"type":"text","text":"hold"}]}`), json.RawMessage(`{"name":"mesh","version":"test"}`), 0)
				responder.Complete(reply, nil)
				close(blockerEntered)
				<-blockerDone
				return
			}
			// Emit progress and wait for cancel.
			responder.EnqueueProgress([]byte(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"p","progress":1}}`))
			notifySent.Done()
			<-ctx.Done()
			responder.Complete(nil, nil)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	inR, inW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- engine.Serve(t.Context(), inR, gated)
	}()

	const meta = `"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}`

	// Block the writer inside gated.Write with one request.
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":"hold","method":"tools/call","params":{"name":"hold_writer","arguments":{},"_meta":{`+meta+`}}}`+"\n")
	<-blockerEntered
	<-gated.entered

	// Queue a non-progress notification on a listen stream, then cancel it before writer resumes.
	notifySent.Add(1)
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":"sub-drop","method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},"_meta":{`+meta+`}}}`+"\n")
	notifySent.Wait()
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"sub-drop"}}`+"\n")

	// Send 1,000 sequential requests that emit progress and get cancelled while stdout is paused.
	for i := range 1000 {
		notifySent.Add(1)
		reqID := fmt.Sprintf("prog-%d", i)
		_, _ = io.WriteString(inW, fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"tools/call","params":{"name":"prog","arguments":{},"_meta":{%s,"progressToken":"p"}}}`, reqID, meta)+"\n")
		notifySent.Wait()
		_, _ = io.WriteString(inW, fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%q}}`, reqID)+"\n")
	}

	// Verify progress queue and owned notification queue are completely drained before writer even resumes!
	deadline := time.Now().Add(3 * time.Second)
	for {
		engine.mu.Lock()
		pLen := engine.progressList.Len()
		eLen := len(engine.progressElem)
		qLen := len(engine.queuedFrames)
		engine.mu.Unlock()
		if pLen == 0 && eLen == 0 && qLen == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retained cancelled entries with paused writer: progressList=%d progressElem=%d queuedFrames=%d", pLen, eLen, qLen)
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(blockerDone)
	close(gated.release)
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}

	for _, line := range gated.snapshot() {
		if strings.Contains(string(line), "must-be-dropped") {
			t.Fatalf("cancelled request notification was emitted after cancel: %s", line)
		}
	}
}
