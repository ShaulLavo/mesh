package mcpnative

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpwire"
)

const (
	testHostID    = "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"
	testInstance  = "0123456789abcdef0123456789abcdef"
	testContextID = "fedcba9876543210fedcba9876543210"
	testContext   = "mc1." + testHostID + "." + testContextID
	testJob       = "mj1." + testHostID + ".7K3D." + testInstance
)

func TestHandleRoundTripsAndValidation(t *testing.T) {
	parsedCtx, err := ParseContextHandle(testContext)
	if err != nil || parsedCtx.HostID != testHostID || parsedCtx.ContextID != testContextID || parsedCtx.String() != testContext {
		t.Fatalf("ParseContextHandle = %+v, %v", parsedCtx, err)
	}
	for _, bad := range []string{
		"", "mc1", "mc1.host", testJob,
		"mc1.bad!host." + testContextID,
		"mc1." + testHostID + ".short",
		"mc1." + testHostID + ".FEDCBA9876543210FEDCBA9876543210",
	} {
		if _, err := ParseContextHandle(bad); !errors.Is(err, ErrInvalidHandle) {
			t.Errorf("ParseContextHandle(%q) = %v, want ErrInvalidHandle", bad, err)
		}
	}

	job, err := FormatJobHandle(testHostID, "7K3D", testInstance)
	if err != nil || job != testJob {
		t.Fatalf("FormatJobHandle = %q, %v", job, err)
	}
	parsedJob, err := ParseJobHandle(job)
	if err != nil || parsedJob.HostID != testHostID || parsedJob.SessionID != "7K3D" || parsedJob.Instance != testInstance {
		t.Fatalf("ParseJobHandle = %+v, %v", parsedJob, err)
	}
	for _, bad := range []string{
		"", "mj1", "mj1.host.7K3D", "mc1." + testHostID + ".7K3D." + testInstance,
		"mj1.bad!host.7K3D." + testInstance,
		"mj1." + testHostID + ".bad_session." + testInstance,
		"mj1." + testHostID + ".7K3D.0123456789ABCDEF0123456789ABCDEF",
		"mj1." + testHostID + ".7K3D.short",
	} {
		if _, err := ParseJobHandle(bad); !errors.Is(err, ErrInvalidHandle) {
			t.Errorf("ParseJobHandle(%q) = %v, want ErrInvalidHandle", bad, err)
		}
	}
}

func TestOutputEncodingAndControlEscaping(t *testing.T) {
	enc, data := EncodeOutputBytes([]byte("hello\x1b[31mred\x1b[0m\n"))
	if enc != "utf-8" || !strings.Contains(data, "hello") {
		t.Fatalf("utf-8 encode = %q, %q", enc, data)
	}
	escaped := escapeControlText(data)
	if strings.Contains(escaped, "\x1b") || !strings.Contains(escaped, "\\u001b[31mred") {
		t.Fatalf("control escape failed: %q", escaped)
	}
	bin := []byte{0x00, 0xff, 0x01}
	enc, data = EncodeOutputBytes(bin)
	if enc != "base64" || data != base64.StdEncoding.EncodeToString(bin) {
		t.Fatalf("binary encode = %q, %q", enc, data)
	}
}

type stubControls struct {
	waitFn        func(ctx context.Context, req JobWaitRequest, notify func(ProgressUpdate)) (CallOutcome, error)
	statusFn      func(ctx context.Context, req JobRefRequest) (CallOutcome, error)
	outputFn      func(ctx context.Context, req JobOutputRequest) (CallOutcome, error)
	cancelFn      func(ctx context.Context, req JobRefRequest) (CallOutcome, error)
	hostsFn       func(ctx context.Context, req ListHostsRequest) (CallOutcome, error)
	openContextFn func(ctx context.Context, req OpenContextRequest) (CallOutcome, error)
	updateCtxFn   func(ctx context.Context, req UpdateContextRequest) (CallOutcome, error)
	execFn        func(ctx context.Context, req ExecRequest, notify func(ProgressUpdate)) (CallOutcome, error)
	fileStub
}

func (s *stubControls) ListHosts(ctx context.Context, req ListHostsRequest) (CallOutcome, error) {
	if s.hostsFn != nil {
		return s.hostsFn(ctx, req)
	}
	id := testHostID
	return CallOutcome{
		Envelope: okEnvelope("hosts.list", agentapi.HostsData{
			Hosts: []agentapi.HostSummary{{HostID: &id, MachineName: "pc", Reachability: "reachable", Capabilities: &agentapi.HostCapabilities{ExecutionPipe: true}}},
		}),
	}, nil
}
func (s *stubControls) InspectHost(context.Context, InspectHostRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("hosts.inspect", agentapi.HostDetail{})}, nil
}
func (s *stubControls) WakeHost(context.Context, WakeHostRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("hosts.wake", WakeResult{HostID: testHostID, AlreadyOnline: true})}, nil
}
func (s *stubControls) ListSessions(context.Context, ListSessionsRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("sessions.list", agentapi.SessionPage{HostID: testHostID, Sessions: []agentapi.SessionSummary{}})}, nil
}
func (s *stubControls) InspectSession(context.Context, InspectSessionRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("sessions.inspect", agentapi.SessionDetail{})}, nil
}
func (s *stubControls) ListServices(context.Context, ListServicesRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("services.list", agentapi.ServicesData{Services: []agentapi.ServiceRow{}, HostErrors: []agentapi.Error{}})}, nil
}
func (s *stubControls) InspectService(context.Context, InspectServiceRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("services.inspect", ServiceDetail{})}, nil
}
func (s *stubControls) OpenContext(ctx context.Context, req OpenContextRequest) (CallOutcome, error) {
	if s.openContextFn != nil {
		return s.openContextFn(ctx, req)
	}
	target := &TargetMeta{HostID: testHostID, HostName: "pc", Account: "agent", ContextID: testContext, Cwd: req.Cwd}
	return CallOutcome{
		Envelope: okEnvelope("contexts.open", agentapi.ExecutionContext{
			HostID: testHostID, ContextID: testContextID, Handle: testContext, RequestKey: req.RequestKey,
			State: "open", Revision: 1, Cwd: req.Cwd, Shell: req.Shell, Label: req.Label, EnvNames: []string{},
		}),
		Target: target,
	}, nil
}
func (s *stubControls) GetContext(_ context.Context, req ContextRefRequest) (CallOutcome, error) {
	target := &TargetMeta{HostID: req.Context.HostID, HostName: "pc", Account: "agent", ContextID: req.Context.String(), Cwd: "/work"}
	return CallOutcome{
		Envelope: okEnvelope("contexts.get", agentapi.ExecutionContext{
			HostID: req.Context.HostID, ContextID: req.Context.ContextID, Handle: req.Context.String(),
			State: "open", Revision: 1, Cwd: "/work", Shell: "login", EnvNames: []string{},
		}),
		Target: target,
	}, nil
}
func (s *stubControls) ListContexts(context.Context, ListContextsRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("contexts.list", agentapi.ExecutionContexts{HostID: testHostID, Contexts: []agentapi.ExecutionContext{}})}, nil
}
func (s *stubControls) ContextReceipt(_ context.Context, req ContextReceiptRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("contexts.receipt", agentapi.ExecutionContext{HostID: testHostID, ContextID: testContextID, Handle: testContext, RequestKey: req.RequestKey, State: "open", Revision: 1, Cwd: "/work", Shell: "login", EnvNames: []string{}})}, nil
}
func (s *stubControls) UpdateContext(ctx context.Context, req UpdateContextRequest) (CallOutcome, error) {
	if s.updateCtxFn != nil {
		return s.updateCtxFn(ctx, req)
	}
	target := &TargetMeta{HostID: req.Context.HostID, HostName: "pc", Account: "agent", ContextID: req.Context.String(), Cwd: req.Cwd}
	return CallOutcome{
		Envelope: okEnvelope("contexts.update", agentapi.ExecutionContext{
			HostID: req.Context.HostID, ContextID: req.Context.ContextID, Handle: req.Context.String(),
			State: "open", Revision: req.ExpectedRevision + 1, Cwd: req.Cwd, Shell: "login", EnvNames: []string{},
		}),
		Target: target,
	}, nil
}
func (s *stubControls) CloseContext(_ context.Context, req CloseContextRequest) (CallOutcome, error) {
	target := &TargetMeta{HostID: req.Context.HostID, HostName: "pc", Account: "agent", ContextID: req.Context.String(), Cwd: "/work"}
	return CallOutcome{
		Envelope: okEnvelope("contexts.close", agentapi.ExecutionContext{
			HostID: req.Context.HostID, ContextID: req.Context.ContextID, Handle: req.Context.String(),
			State: "closed", Revision: 2, Cwd: "/work", Shell: "login", EnvNames: []string{},
		}),
		Target: target,
	}, nil
}
func (s *stubControls) Exec(ctx context.Context, req ExecRequest, notify func(ProgressUpdate)) (CallOutcome, error) {
	if s.execFn != nil {
		return s.execFn(ctx, req, notify)
	}
	code := 0
	target := &TargetMeta{HostID: req.Context.HostID, HostName: "pc", Account: "agent", ContextID: req.Context.String(), Cwd: "/work"}
	return CallOutcome{
		Envelope: okEnvelope("sessions.create", ExecResultData{
			Job: testJob, RequestKey: req.RequestKey, Target: *target, State: "exited", Wait: "completed",
			Result: &agentapi.ExecutionResult{Termination: agentapi.ExecutionTermination{Kind: "exit", Code: &code}},
			Stdout: &ExecStreamData{Encoding: "utf-8", Data: "ok\n", Start: "0", End: "3", NextCursor: "c1", EOF: true},
			Stderr: &ExecStreamData{Encoding: "utf-8", Data: "", Start: "0", End: "0", NextCursor: "c2", EOF: true},
		}),
		Target: target,
	}, nil
}
func (s *stubControls) JobStatus(ctx context.Context, req JobRefRequest) (CallOutcome, error) {
	if s.statusFn != nil {
		return s.statusFn(ctx, req)
	}
	target := &TargetMeta{HostID: req.Job.HostID, HostName: "pc", Account: "agent"}
	return CallOutcome{
		Envelope: okEnvelope("sessions.inspect", JobStateData{
			Job: req.Job.String(), Target: *target, State: "running",
			Receipt: &agentapi.ExecutionReceipt{HostID: req.Job.HostID, SessionID: req.Job.SessionID, Instance: req.Job.Instance, Phase: "live"},
		}),
		Target: target,
	}, nil
}
func (s *stubControls) JobWait(ctx context.Context, req JobWaitRequest, notify func(ProgressUpdate)) (CallOutcome, error) {
	if s.waitFn != nil {
		return s.waitFn(ctx, req, notify)
	}
	return s.JobStatus(ctx, JobRefRequest{Job: req.Job})
}
func (s *stubControls) JobOutput(ctx context.Context, req JobOutputRequest) (CallOutcome, error) {
	if s.outputFn != nil {
		return s.outputFn(ctx, req)
	}
	target := &TargetMeta{HostID: req.Job.HostID, HostName: "pc", Account: "agent"}
	return CallOutcome{
		Envelope: okEnvelope("sessions.logs", JobOutputData{Job: req.Job.String(), Target: *target, Stream: req.Stream, Encoding: "utf-8", Data: "hello\n", EOF: true}),
		Target:   target,
	}, nil
}
func (s *stubControls) JobReceipt(context.Context, JobReceiptRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("sessions.request", JobStateData{Job: testJob, State: "exited"})}, nil
}
func (s *stubControls) ListJobs(context.Context, ListJobsRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("jobs.list", JobListData{HostID: testHostID, Jobs: []JobListItem{}})}, nil
}
func (s *stubControls) JobInput(context.Context, JobInputRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("sessions.input", JobInputData{Job: testJob, WriteKey: "k1", Offset: "0", Length: 2})}, nil
}
func (s *stubControls) JobSignal(context.Context, JobSignalRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("sessions.signal", JobStateData{Job: testJob, State: "running", Delivery: "delivered"})}, nil
}
func (s *stubControls) JobCancel(ctx context.Context, req JobRefRequest) (CallOutcome, error) {
	if s.cancelFn != nil {
		return s.cancelFn(ctx, req)
	}
	target := &TargetMeta{HostID: req.Job.HostID, HostName: "pc", Account: "agent"}
	return CallOutcome{
		Envelope: okEnvelope("sessions.kill", JobStateData{Job: req.Job.String(), Target: *target, State: "killed", Delivery: "delivered"}),
		Target:   target,
	}, nil
}
func (s *stubControls) JobRemove(context.Context, JobRefRequest) (CallOutcome, error) {
	return CallOutcome{Envelope: okEnvelope("sessions.remove", JobStateData{Job: testJob, State: "exited"})}, nil
}

func startTestServer(t *testing.T, opts ServerOptions) (inWriter *io.PipeWriter, outReader *mcpwire.StdioReader, stderr *syncBuffer, done <-chan error) {
	t.Helper()
	stderr = &syncBuffer{}
	opts.Stderr = stderr
	server, err := NewStdioServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	doneCh := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		doneCh <- server.Serve(t.Context(), serverIn, serverOut)
		_ = serverOut.Close()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = clientOut.Close()
		_ = clientIn.Close()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("stdio server did not exit")
		}
	})
	return clientOut, mcpwire.NewStdioReader(clientIn), stderr, doneCh
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("write test stderr buffer: %w", err)
	}
	return n, nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func mcpReq(id, method, extra string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}` + extra + `}}`)
}

func TestStdioFreshCallDiscoverLegacyRefusalAndErrors(t *testing.T) {
	out, reader, stderr, _ := startTestServer(t, ServerOptions{Version: "0.1.0-test", Controls: &stubControls{}})

	// 1. Fresh frontend reads a job as its very first call without discovery.
	callFirst := mcpReq("1", "tools/call", `,"name":"mesh_job_status","arguments":{"job":"`+testJob+`"}`)
	if err := mcpwire.WriteStdio(out, callFirst); err != nil {
		t.Fatal(err)
	}
	raw, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	var firstResp struct {
		ID     int `json:"id"`
		Result struct {
			ResultType        string            `json:"resultType"`
			IsError           bool              `json:"isError"`
			StructuredContent agentapi.Envelope `json:"structuredContent"`
			Meta              map[string]any    `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &firstResp); err != nil {
		t.Fatal(err)
	}
	if firstResp.ID != 1 || firstResp.Result.ResultType != "complete" || firstResp.Result.IsError || firstResp.Result.StructuredContent.Status != agentapi.StatusOK {
		t.Fatalf("fresh job call failed: %s", raw)
	}
	if firstResp.Result.Meta[MetaTargetKey] == nil || firstResp.Result.Meta[mcpwire.ServerInfoKey] == nil {
		t.Fatalf("missing _meta keys: %s", raw)
	}

	// 2. server/discover advertises static tools and points to mesh_skill.
	if err := mcpwire.WriteStdio(out, mcpReq(`"disc"`, "server/discover", "")); err != nil {
		t.Fatal(err)
	}
	raw, err = reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`mesh_skill`)) || !bytes.Contains(raw, []byte(`"2026-07-28"`)) || !bytes.Contains(raw, []byte(`"listChanged":false`)) {
		t.Fatalf("unexpected discover reply: %s", raw)
	}

	// 3. Legacy initialize is refused with -32601 naming 2026-07-28.
	legacy := []byte(`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"old","version":"1"}}}`)
	if err := mcpwire.WriteStdio(out, legacy); err != nil {
		t.Fatal(err)
	}
	raw, err = reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"code":-32601`)) || !bytes.Contains(raw, []byte("2026-07-28")) {
		t.Fatalf("legacy initialize not refused cleanly: %s", raw)
	}

	// 4. Malformed handle gets -32602.
	badHandle := mcpReq("8", "tools/call", `,"name":"mesh_job_status","arguments":{"job":"bad-handle"}`)
	if err := mcpwire.WriteStdio(out, badHandle); err != nil {
		t.Fatal(err)
	}
	raw, err = reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"code":-32602`)) {
		t.Fatalf("malformed handle did not get -32602: %s", raw)
	}

	// 5. Unknown notification logs only method name, never payload.
	secretNote := []byte(`{"jsonrpc":"2.0","method":"notifications/custom_ignored","params":{"secret":"top-secret-value"}}`)
	if err := mcpwire.WriteStdio(out, secretNote); err != nil {
		t.Fatal(err)
	}
	// Follow with a ping-equivalent discover to flush reader ordering.
	if err := mcpwire.WriteStdio(out, mcpReq("9", "tools/list", "")); err != nil {
		t.Fatal(err)
	}
	raw, err = reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"mesh_hosts_list"`)) {
		t.Fatalf("tools/list missing expected tool: %s", raw)
	}
	logs := stderr.String()
	if !strings.Contains(logs, "notifications/custom_ignored") || strings.Contains(logs, "top-secret-value") {
		t.Fatalf("ignored notification stderr leak: %q", logs)
	}
}

func TestStdioCapacityCancellationProgressAndEOF(t *testing.T) {
	waitStarted := make(chan struct{}, 2)
	waitCancelled := make(chan struct{}, 2)
	var cancelToolCalled atomic.Int32
	var shutdownCalled atomic.Int32

	controls := &stubControls{
		waitFn: func(ctx context.Context, req JobWaitRequest, notify func(ProgressUpdate)) (CallOutcome, error) {
			if notify != nil {
				notify(ProgressUpdate{Progress: 1, Message: "running"})
			}
			waitStarted <- struct{}{}
			<-ctx.Done()
			waitCancelled <- struct{}{}
			return CallOutcome{}, ctx.Err()
		},
		cancelFn: func(_ context.Context, req JobRefRequest) (CallOutcome, error) {
			cancelToolCalled.Add(1)
			target := &TargetMeta{HostID: req.Job.HostID}
			return CallOutcome{
				Envelope: okEnvelope("sessions.kill", JobStateData{Job: req.Job.String(), Target: *target, State: "killed", Delivery: "delivered"}),
				Target:   target,
			}, nil
		},
	}

	out, reader, _, done := startTestServer(t, ServerOptions{
		Version:     "0.1.0-test",
		Controls:    controls,
		MaxRequests: 2,
		OnShutdown:  func() { shutdownCalled.Add(1) },
	})

	// Fill both slots with waits (first includes progressToken).
	req1 := []byte(`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"progressToken":"p1"},"name":"mesh_job_wait","arguments":{"job":"` + testJob + `"}}}`)
	req2 := mcpReq("11", "tools/call", `,"name":"mesh_job_wait","arguments":{"job":"`+testJob+`"}`)
	if err := mcpwire.WriteStdio(out, req1); err != nil {
		t.Fatal(err)
	}
	if err := mcpwire.WriteStdio(out, req2); err != nil {
		t.Fatal(err)
	}
	<-waitStarted
	<-waitStarted

	// Progress notification arrives for request 10.
	progRaw, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(progRaw, []byte(`"notifications/progress"`)) || !bytes.Contains(progRaw, []byte(`"progressToken":"p1"`)) {
		t.Fatalf("expected progress notification, got: %s", progRaw)
	}

	// Duplicate active ID is refused with id:null while keeping request 10 alive.
	if err := mcpwire.WriteStdio(out, mcpReq("10", "tools/list", "")); err != nil {
		t.Fatal(err)
	}
	dupRaw, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(dupRaw, []byte(`"id":null`)) || !bytes.Contains(dupRaw, []byte(`"code":-32600`)) {
		t.Fatalf("duplicate active ID reply = %s", dupRaw)
	}

	// 3rd request at capacity gets correlated -31000 resource.limit.
	if err := mcpwire.WriteStdio(out, mcpReq("12", "tools/list", "")); err != nil {
		t.Fatal(err)
	}
	capRaw, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(capRaw, []byte(`"id":12`)) || !bytes.Contains(capRaw, []byte(`"code":-31000`)) || !bytes.Contains(capRaw, []byte(`"resource.limit"`)) {
		t.Fatalf("capacity error = %s", capRaw)
	}

	// While slots are full, cancel wait 10 -> frees slot 10 without calling mesh_job_cancel.
	cancel10 := []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":10,"reason":"user cancelled wait"}}`)
	if err := mcpwire.WriteStdio(out, cancel10); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waitCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("wait 10 was not cancelled at capacity")
	}
	if cancelToolCalled.Load() != 0 {
		t.Fatal("cancelling a wait called JobCancel")
	}

	// Now explicitly cancel the job through mesh_job_cancel in the freed slot.
	// Retry once briefly if finishActive is still releasing the slot.
	var cancelReply []byte
	for attempt := 0; attempt < 20; attempt++ {
		reqID := string(rune('a' + attempt))
		if err := mcpwire.WriteStdio(out, mcpReq(`"`+reqID+`"`, "tools/call", `,"name":"mesh_job_cancel","arguments":{"job":"`+testJob+`"}`)); err != nil {
			t.Fatal(err)
		}
		raw, err := reader.Read()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(`"resource.limit"`)) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		cancelReply = raw
		break
	}
	if !bytes.Contains(cancelReply, []byte(`"killed"`)) || cancelToolCalled.Load() != 1 {
		t.Fatalf("explicit job cancel failed: %s (count=%d)", cancelReply, cancelToolCalled.Load())
	}

	// Close stdin while wait 11 is still active -> exits cleanly within 2s and invokes OnShutdown.
	_ = out.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("EOF exit error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not exit on stdin EOF")
	}
	if shutdownCalled.Load() != 1 {
		t.Fatalf("OnShutdown count = %d, want 1", shutdownCalled.Load())
	}
}

func TestStdioOversizedClientAndReply(t *testing.T) {
	controls := &stubControls{
		outputFn: func(_ context.Context, req JobOutputRequest) (CallOutcome, error) {
			return CallOutcome{
				Envelope: okEnvelope("sessions.logs", JobOutputData{
					Job: req.Job.String(), Stream: req.Stream, Encoding: "utf-8",
					Data: strings.Repeat("A", mcpwire.MaxMessage),
				}),
			}, nil
		},
	}
	out, reader, _, _ := startTestServer(t, ServerOptions{Version: "0.1.0-test", Controls: controls})

	// Oversized client line (>3.5 MiB) with a verifiable JSON-RPC request envelope
	// preserves its ID in the -32600 fault.
	huge := append([]byte(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"pad":"`), bytes.Repeat([]byte("x"), mcpwire.MaxMessage+1024)...)
	huge = append(huge, []byte(`"}}`+"\n")...)
	if _, err := out.Write(huge); err != nil {
		t.Fatal(err)
	}
	raw, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"id":1`)) || !bytes.Contains(raw, []byte(`"code":-32600`)) {
		t.Fatalf("oversized client reply = %s", raw)
	}

	// Unverifiable oversized garbage gets id:null.
	garbage := append(bytes.Repeat([]byte("z"), mcpwire.MaxMessage+1024), '\n')
	if _, err := out.Write(garbage); err != nil {
		t.Fatal(err)
	}
	raw, err = reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"id":null`)) || !bytes.Contains(raw, []byte(`"code":-32600`)) {
		t.Fatalf("oversized garbage reply = %s", raw)
	}

	// Oversized reply becomes correlated -31000 mcp.message_too_large.
	if err := mcpwire.WriteStdio(out, mcpReq("2", "tools/call", `,"name":"mesh_job_output","arguments":{"job":"`+testJob+`"}`)); err != nil {
		t.Fatal(err)
	}
	raw, err = reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"id":2`)) || !bytes.Contains(raw, []byte(`"code":-31000`)) || !bytes.Contains(raw, []byte(`"mcp.message_too_large"`)) {
		t.Fatalf("oversized server reply = %s", raw)
	}
}
