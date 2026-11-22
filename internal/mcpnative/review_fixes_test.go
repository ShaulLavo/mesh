package mcpnative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
)

type gatedWriter struct {
	mu      sync.Mutex
	writes  [][]byte
	entered chan struct{}
	release chan struct{}
	failErr error
}

func newGatedWriter() *gatedWriter {
	return &gatedWriter{
		entered: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failErr != nil {
		return 0, w.failErr
	}
	w.writes = append(w.writes, bytes.Clone(p))
	return len(p), nil
}

func (w *gatedWriter) snapshot() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([][]byte, len(w.writes))
	copy(out, w.writes)
	return out
}

type blockingWriter struct {
	unblock chan struct{}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	<-w.unblock
	return len(p), nil
}

func TestHandleRejectsShortMachineNameAndOversizedInput(t *testing.T) {
	for _, bad := range []string{
		"mj1.pc.7K3D." + testInstance,
		"mj1." + strings.Repeat("a", 5000) + ".7K3D." + testInstance,
		"mj1.shortHostID.7K3D." + testInstance,
	} {
		if _, err := ParseJobHandle(bad); !errors.Is(err, ErrInvalidHandle) {
			t.Fatalf("ParseJobHandle(%q) = %v, want ErrInvalidHandle", bad, err)
		}
	}
}

func TestStdioTruncatedLineReturnsFramingLoss(t *testing.T) {
	server, err := NewStdioServer(ServerOptions{Version: "0.1.0-test", Controls: &stubControls{}, EOFDrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = server.Serve(t.Context(), strings.NewReader(`{"jsonrpc":"2.0","id":1`), &out)
	if !errors.Is(err, mcpwire.ErrFramingLost) {
		t.Fatalf("Serve error = %v, want ErrFramingLost", err)
	}
}

func TestStdioShutdownDeadlineBoundsStuckWriterAndShutdownHook(t *testing.T) {
	stuckOut := &blockingWriter{unblock: make(chan struct{})}
	hookUnblock := make(chan struct{})
	defer close(stuckOut.unblock)
	defer close(hookUnblock)

	server, err := NewStdioServer(ServerOptions{
		Version:         "0.1.0-test",
		Controls:        &stubControls{},
		EOFDrainTimeout: 150 * time.Millisecond,
		OnShutdown: func() {
			<-hookUnblock
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader(string(mcpReq("1", "tools/call", `,"name":"mesh_skill","arguments":{}`)) + "\n")
	start := time.Now()
	_ = server.Serve(t.Context(), in, stuckOut)
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatalf("Serve took %s with stuck stdout and shutdown hook, want <= 1s", elapsed)
	}
}

func TestStdioByteBudgetsChargeFaultsAndInDeliveryFrames(t *testing.T) {
	writer := newGatedWriter()
	server, err := NewStdioServer(ServerOptions{
		Version:          "0.1.0-test",
		Controls:         &stubControls{},
		MaxFrontendBytes: 64 << 10,
		MaxConnBytes:     2048,
		EOFDrainTimeout:  300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	serverIn, clientOut := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(t.Context(), serverIn, writer)
	}()

	// Send one request whose reply blocks in writer.Write; its bytes stay charged.
	if err := mcpwire.WriteStdio(clientOut, mcpReq("1", "server/discover", "")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writer.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not start writing first reply")
	}

	// Flood invalid JSON lines so unadmitted faults must charge against MaxConnBytes
	// and close the channel immediately while stdout is still blocked.
	go func() {
		for range 128 {
			if _, writeErr := clientOut.Write([]byte("not-json\n")); writeErr != nil {
				return
			}
		}
	}()

	select {
	case serveErr := <-done:
		close(writer.release)
		_ = clientOut.Close()
		if !errors.Is(serveErr, errOutputCapacity) {
			t.Fatalf("Serve error = %v, want errOutputCapacity when faults overflow MaxConnBytes", serveErr)
		}
	case <-time.After(3 * time.Second):
		close(writer.release)
		_ = clientOut.Close()
		t.Fatal("server did not close channel on fault byte overflow")
	}
}

func TestStdioBlockedStderrNeverStallsCancellation(t *testing.T) {
	blockedStderr := &blockingWriter{unblock: make(chan struct{})}
	defer close(blockedStderr.unblock)

	started := make(chan struct{})
	cancelled := make(chan struct{})
	controls := &stubControls{
		waitFn: func(ctx context.Context, _ JobWaitRequest, _ func(ProgressUpdate)) (CallOutcome, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return CallOutcome{}, ctx.Err()
		},
	}
	server, err := NewStdioServer(ServerOptions{
		Version:         "0.1.0-test",
		Controls:        controls,
		Stderr:          blockedStderr,
		EOFDrainTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(t.Context(), serverIn, serverOut)
		_ = serverOut.Close()
	}()
	t.Cleanup(func() {
		_ = clientOut.Close()
		_ = clientIn.Close()
		<-done
	})

	if err := mcpwire.WriteStdio(clientOut, mcpReq(`"wait-1"`, "tools/call", `,"name":"mesh_job_wait","arguments":{"job":"`+testJob+`","wait_ms":5000}`)); err != nil {
		t.Fatal(err)
	}
	<-started

	// Fill stderr queue with ignored notifications while stderr writer is blocked.
	for range 32 {
		if err := mcpwire.WriteStdio(clientOut, []byte(`{"jsonrpc":"2.0","method":"notifications/custom","params":{}}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := mcpwire.WriteStdio(clientOut, []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"wait-1"}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked stderr stalled cancellation delivery")
	}
}

func TestStdioCanonicalIDCancellationActiveThroughDeliveryAndOversizedID(t *testing.T) {
	// 1. Canonical JSON string ID matching: request uses "\u0078", cancel uses "x".
	started := make(chan struct{})
	cancelled := make(chan struct{})
	lateNotify := make(chan func(ProgressUpdate), 1)
	controls := &stubControls{
		waitFn: func(ctx context.Context, _ JobWaitRequest, notify func(ProgressUpdate)) (CallOutcome, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return CallOutcome{}, ctx.Err()
		},
		statusFn: func(_ context.Context, req JobRefRequest) (CallOutcome, error) {
			target := &TargetMeta{HostID: req.Job.HostID, HostName: "pc"}
			return CallOutcome{
				Envelope: okEnvelope("sessions.inspect", JobStateData{Job: req.Job.String(), Target: *target, State: "running"}),
				Target:   target,
			}, nil
		},
	}
	out, reader, _, _ := startTestServer(t, ServerOptions{Version: "0.1.0-test", Controls: controls})

	escapedReq := []byte(`{"jsonrpc":"2.0","id":"\u0078","method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},"name":"mesh_job_wait","arguments":{"job":"` + testJob + `","wait_ms":5000}}}`)
	if err := mcpwire.WriteStdio(out, escapedReq); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := mcpwire.WriteStdio(out, []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"x"}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal(`cancel with "x" did not match active request "\u0078"`)
	}

	// 2. Oversized request with valid ID preserves the ID in the -32600 fault.
	bigPad := strings.Repeat("a", mcpwire.MaxMessage+64)
	oversizedLine := `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"mesh_skill","arguments":{"file":"` + bigPad + `"}}}` + "\n"
	if _, err := out.Write([]byte(oversizedLine)); err != nil {
		t.Fatal(err)
	}
	raw, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"id":42`)) || !bytes.Contains(raw, []byte(`-32600`)) {
		t.Fatalf("oversized request reply did not preserve id 42: %s", raw)
	}

	// 3. Active ID stays reserved until terminal write completes, and late progress cannot publish after completion.
	writer := newGatedWriter()
	gatedControls := &stubControls{
		waitFn: func(_ context.Context, req JobWaitRequest, notify func(ProgressUpdate)) (CallOutcome, error) {
			lateNotify <- notify
			return CallOutcome{
				Envelope: okEnvelope("sessions.wait", JobStateData{Job: req.Job.String(), State: "exited"}),
			}, nil
		},
	}
	gatedServer, err := NewStdioServer(ServerOptions{Version: "0.1.0-test", Controls: gatedControls, EOFDrainTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	serverIn, clientOut := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- gatedServer.Serve(t.Context(), serverIn, writer)
	}()

	req1 := mcpReq(`"reuse-id"`, "tools/call", `,"name":"mesh_job_wait","arguments":{"job":"`+testJob+`","wait_ms":1000}`)
	req1 = bytes.Replace(req1, []byte(`"clientCapabilities":{}`), []byte(`"clientCapabilities":{},"progressToken":"pt-late"`), 1)
	if err := mcpwire.WriteStdio(clientOut, req1); err != nil {
		t.Fatal(err)
	}
	<-writer.entered
	// Invoke notify AFTER completion; must not enqueue any progress notification.
	(<-lateNotify)(ProgressUpdate{Progress: 99, Message: "late"})

	// While first response is still in writer.Write, reusing "reuse-id" must fail as duplicate active ID.
	if err := mcpwire.WriteStdio(clientOut, mcpReq(`"reuse-id"`, "server/discover", "")); err != nil {
		t.Fatal(err)
	}
	// Give reader a moment to process duplicate before releasing writer.
	time.Sleep(50 * time.Millisecond)
	close(writer.release)

	// Wait until first reply and duplicate error are written, then reuse "reuse-id" again: now it succeeds.
	deadline := time.Now().Add(2 * time.Second)
	for len(writer.snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := mcpwire.WriteStdio(clientOut, mcpReq(`"reuse-id"`, "server/discover", "")); err != nil {
		t.Fatal(err)
	}
	for len(writer.snapshot()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = clientOut.Close()
	<-done

	writes := writer.snapshot()
	if len(writes) != 3 {
		t.Fatalf("got %d writes (want 3: first reply, duplicate error, post-delivery reuse): %q", len(writes), writes)
	}
	for _, w := range writes {
		if bytes.Contains(w, []byte(`notifications/progress`)) {
			t.Fatalf("late progress published after completion: %s", w)
		}
	}
	if !bytes.Contains(writes[1], []byte(`"id":null`)) || !bytes.Contains(writes[1], []byte(`Request ID is already active`)) {
		t.Fatalf("expected duplicate active ID rejection while reply in-flight, got: %s", writes[1])
	}
	if !bytes.Contains(writes[2], []byte(`"id":"reuse-id"`)) || !bytes.Contains(writes[2], []byte(`"resultType":"complete"`)) {
		t.Fatalf("expected reuse after delivery to succeed, got: %s", writes[2])
	}
}

func TestStdioToolHandlerErrorsUseAgentEnvelope(t *testing.T) {
	controls := &stubControls{
		hostsFn: func(context.Context, ListHostsRequest) (CallOutcome, error) {
			return CallOutcome{}, errors.New("load hosts.json: permission denied")
		},
	}
	out, reader, _, _ := startTestServer(t, ServerOptions{Version: "0.1.0-test", Controls: controls})

	if err := mcpwire.WriteStdio(out, mcpReq("1", "tools/call", `,"name":"mesh_hosts_list","arguments":{}`)); err != nil {
		t.Fatal(err)
	}
	raw, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Error  any `json:"error"`
		Result struct {
			IsError           bool              `json:"isError"`
			StructuredContent agentapi.Envelope `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("plain tool failure returned JSON-RPC error instead of agent envelope: %s", raw)
	}
	if !resp.Result.IsError || resp.Result.StructuredContent.Status != agentapi.StatusError || len(resp.Result.StructuredContent.Errors) != 1 || resp.Result.StructuredContent.Errors[0].Code != agentapi.StateUnavailable {
		t.Fatalf("unexpected tool error envelope: %s", raw)
	}
}
