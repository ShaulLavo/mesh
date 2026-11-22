package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func testTerminalResult() *protocol.ExecutionResult {
	code := 0
	return &protocol.ExecutionResult{
		Termination: protocol.ExecutionTermination{Kind: "exit", Code: &code},
		Capture:     "complete",
	}
}

func TestMCPPart2WaitOutputErrorsSlotDeadlineAndMonotonicProgress(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(dir, "cfg"))
	t.Setenv("MESH_STATE_DIR", filepath.Join(dir, "state"))

	hostID := "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"
	ctxID := "0123456789abcdef0123456789abcdef"
	inst := "11111111111111111111111111111111"
	host := HostRecord{ID: hostID, MeshIdentity: hostID, MachineName: "pc", Endpoint: "ws://127.0.0.1:1/control/ws"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	app := &application{dependencies: Dependencies{Now: time.Now}}

	baseReceipt := func(phase string) *protocol.ExecutionReceipt {
		r := &protocol.ExecutionReceipt{
			HostID: hostID, SessionID: "7K3D", Instance: inst, RequestKey: "k1",
			KeyScope: "application", Kind: "pipe", Phase: phase, Acceptance: "durable",
			LaunchOutcome: "started", Publication: "published", Removal: "present",
			ContextID: ctxID,
		}
		if phase == "terminal" {
			r.Result = testTerminalResult()
		}
		return r
	}

	// Finding 2a: host TypeError on wait MUST return IsError=true with propagated code (not success).
	waitErrPool := newMCPHostPool(func(_ context.Context, h HostRecord) (transport.Conn, error) {
		return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
			switch req.Type {
			case protocol.TypeHostInfo:
				return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: "pc", NameRevision: 1, Account: "remote-bob"}}, nil
			case protocol.TypeExecutionCreate:
				return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionReceipt: baseReceipt("live")}, nil
			case protocol.TypeExecutionWait:
				return protocol.Control{Type: protocol.TypeError, ErrorCode: "access.denied", Message: "revoked"}, nil
			default:
				return protocol.Control{Type: protocol.TypeError, ErrorCode: "usage.invalid", Message: "unexpected"}, nil
			}
		}), nil
	}, time.Now)
	defer waitErrPool.Close()
	waitErrControls := newMCPControls(app, waitErrPool, "mac1_test_cred")
	out, err := waitErrControls.Exec(t.Context(), mcpnative.ExecRequest{
		Context: mcpnative.ContextRef{HostID: hostID, ContextID: ctxID}, RequestKey: "k1",
		Argv: []string{"/bin/true"}, Stdin: "closed", WaitMS: 2000, OutputBytes: 4096,
	}, nil)
	if err != nil || !out.IsError || len(out.Envelope.Errors) == 0 || out.Envelope.Errors[0].Code != agentapi.AccessDenied {
		t.Fatalf("Finding 2a: expected IsError access.denied when wait fails, got out=%+v err=%v", out, err)
	}
	if out.Envelope.Data != nil {
		t.Fatalf("Finding 2a: access.denied wait failure must not disclose envelope.Data, got %#v", out.Envelope.Data)
	}

	// Finding 2b: output error (e.g. result.expired) MUST return IsError=true (not success with nil stream).
	logsErrPool := newMCPHostPool(func(_ context.Context, h HostRecord) (transport.Conn, error) {
		return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
			switch req.Type {
			case protocol.TypeHostInfo:
				return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: "pc", NameRevision: 1, Account: "remote-bob"}}, nil
			case protocol.TypeExecutionCreate:
				return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionReceipt: baseReceipt("terminal")}, nil
			case protocol.TypeExecutionLogs:
				return protocol.Control{Type: protocol.TypeError, ErrorCode: "result.expired", Message: "output expired"}, nil
			default:
				return protocol.Control{Type: protocol.TypeError, ErrorCode: "usage.invalid", Message: "unexpected"}, nil
			}
		}), nil
	}, time.Now)
	defer logsErrPool.Close()
	logsErrControls := newMCPControls(app, logsErrPool, "mac1_test_cred")
	logsOut, err := logsErrControls.Exec(t.Context(), mcpnative.ExecRequest{
		Context: mcpnative.ContextRef{HostID: hostID, ContextID: ctxID}, RequestKey: "k1",
		Argv: []string{"/bin/true"}, Stdin: "closed", WaitMS: 0, OutputBytes: 4096,
	}, nil)
	if err != nil || !logsOut.IsError || len(logsOut.Envelope.Errors) == 0 || logsOut.Envelope.Errors[0].Code != agentapi.ResultExpired {
		t.Fatalf("Finding 2b: expected IsError result.expired when logs fail, got out=%+v err=%v", logsOut, err)
	}

	// Finding 3: holding all 4 stream slots causes Exec(wait_ms=100) to expire during slot admission
	// and return timed_out with the accepted receipt WITHOUT sending any execution.wait RPC.
	var waitRPCs atomic.Int32
	slotRelease := make(chan struct{})
	slotEntered := make(chan struct{}, mcpPoolStreamLimit)
	slotPool := newMCPHostPool(func(_ context.Context, h HostRecord) (transport.Conn, error) {
		return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
			switch req.Type {
			case protocol.TypeHostInfo:
				return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: "pc", NameRevision: 1, Account: "remote-bob"}}, nil
			case protocol.TypeExecutionCreate:
				return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionReceipt: baseReceipt("live")}, nil
			case protocol.TypeExecutionWait:
				if req.SessionID == "9Z9Z" {
					slotEntered <- struct{}{}
					<-slotRelease
					return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionWait: "timed_out", ExecutionReceipt: baseReceipt("live")}, nil
				}
				waitRPCs.Add(1)
				return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionWait: "timed_out", ExecutionReceipt: baseReceipt("live")}, nil
			case protocol.TypeExecutionLogs:
				return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionPage: &protocol.ExecutionPage{
					HostID: h.ID, SessionID: req.SessionID, Instance: req.Instance, Stream: req.Stream,
					ObservedEnd: "0", Generation: "1", Available: true,
				}}, nil
			default:
				return protocol.Control{Type: protocol.TypeError, ErrorCode: "usage.invalid", Message: "unexpected"}, nil
			}
		}), nil
	}, time.Now)
	defer slotPool.Close()

	var holders sync.WaitGroup
	for range mcpPoolStreamLimit {
		holders.Add(1)
		go func() {
			defer holders.Done()
			_, _, _ = slotPool.ExchangeStreamBudgeted(t.Context(), host, time.Now().Add(10*time.Second), func(timeoutMS int) protocol.Control {
				return protocol.Control{Type: protocol.TypeExecutionWait, SessionID: "9Z9Z", Instance: inst, TimeoutMillis: timeoutMS}
			})
		}()
	}
	for range mcpPoolStreamLimit {
		<-slotEntered
	}
	slotControls := newMCPControls(app, slotPool, "mac1_test_cred")
	t0 := time.Now()
	slotOut, err := slotControls.Exec(t.Context(), mcpnative.ExecRequest{
		Context: mcpnative.ContextRef{HostID: hostID, ContextID: ctxID}, RequestKey: "k1",
		Argv: []string{"/bin/true"}, Stdin: "closed", WaitMS: 120, OutputBytes: 4096,
	}, nil)
	elapsed := time.Since(t0)
	close(slotRelease)
	holders.Wait()
	if err != nil || slotOut.IsError {
		t.Fatalf("Finding 3: expected timed_out ok outcome on expired slot wait, got out=%+v err=%v", slotOut, err)
	}
	if waitRPCs.Load() != 0 {
		t.Fatalf("Finding 3: expired wait sent %d execution.wait RPCs, want 0", waitRPCs.Load())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Finding 3: expired wait took %v (waited past deadline)", elapsed)
	}

	// Finding 4b: quiet job must NOT emit duplicate progress values (0, 0, 0).
	var probes atomic.Int32
	quietPool := newMCPHostPool(func(_ context.Context, h HostRecord) (transport.Conn, error) {
		return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
			if req.Type == protocol.TypeHostInfo {
				return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: "pc", NameRevision: 1}}, nil
			}
			n := probes.Add(1)
			if n == 3 {
				return protocol.Control{}, errors.New("transient read error")
			}
			return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionPage: &protocol.ExecutionPage{
				HostID: h.ID, SessionID: req.SessionID, Instance: req.Instance, Stream: req.Stream,
				ObservedEnd: "0", Generation: "1", Available: true,
			}}, nil
		}), nil
	}, time.Now)
	defer quietPool.Close()
	quietControls := newMCPControls(app, quietPool, "mac1_test_cred")
	var mu sync.Mutex
	var progressVals []float64
	stop := quietControls.startExecProgress(t.Context(), host, mcpnative.ContextRef{HostID: hostID, ContextID: ctxID}, mcpnative.JobRef{HostID: hostID, SessionID: "7K3D", Instance: inst}, func(u mcpnative.ProgressUpdate) {
		mu.Lock()
		progressVals = append(progressVals, u.Progress)
		mu.Unlock()
	})
	time.Sleep(2200 * time.Millisecond)
	stop()
	mu.Lock()
	vals := append([]float64(nil), progressVals...)
	mu.Unlock()
	for i := 1; i < len(vals); i++ {
		if vals[i] <= vals[i-1] {
			t.Fatalf("Finding 4b: non-increasing progress values on quiet job: %v", vals)
		}
	}

	// Finding 5: context-scoped JobReceipt and ListJobs must NOT fall back to machine names.
	// Put a host named "pc" in hosts.json and craft a ContextRef with an unknown hostID.
	missingHostID := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if out, err := slotControls.JobReceipt(t.Context(), mcpnative.JobReceiptRequest{
		Host: "pc", Context: &mcpnative.ContextRef{HostID: missingHostID, ContextID: ctxID}, RequestKey: "k1", Timeout: time.Second,
	}); err == nil && !out.IsError {
		t.Fatalf("Finding 5: JobReceipt resolved machine name instead of context hostID: %+v", out)
	}
	if out, err := slotControls.ListJobs(t.Context(), mcpnative.ListJobsRequest{
		Host: "pc", Context: &mcpnative.ContextRef{HostID: missingHostID, ContextID: ctxID}, Limit: 10, Timeout: time.Second,
	}); err == nil && !out.IsError {
		t.Fatalf("Finding 5: ListJobs resolved machine name instead of context hostID: %+v", out)
	}

	// Finding 7b: 8,000-byte escaped env value must be rejected deterministically BEFORE sending (not request.outcome_unknown).
	var createCalls atomic.Int32
	bigEnvPool := newMCPHostPool(func(_ context.Context, h HostRecord) (transport.Conn, error) {
		return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
			if req.Type == protocol.TypeHostInfo {
				return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: "pc", NameRevision: 1}}, nil
			}
			createCalls.Add(1)
			return protocol.Control{}, fmt.Errorf("transport: frame rejected by decoder")
		}), nil
	}, time.Now)
	defer bigEnvPool.Close()
	bigEnvControls := newMCPControls(app, bigEnvPool, "mac1_test_cred")
	bigVal := make([]byte, 8000)
	for i := range bigVal {
		bigVal[i] = 0x01
	}
	bigOut, err := bigEnvControls.Exec(t.Context(), mcpnative.ExecRequest{
		Context: mcpnative.ContextRef{HostID: hostID, ContextID: ctxID}, RequestKey: "k-big",
		Argv: []string{"/bin/true"}, Env: map[string]string{"BIG": string(bigVal)}, Stdin: "closed", WaitMS: 1000, OutputBytes: 4096,
	}, nil)
	if err != nil || !bigOut.IsError || len(bigOut.Envelope.Errors) == 0 || bigOut.Envelope.Errors[0].Code != agentapi.UsageInvalid {
		t.Fatalf("Finding 7b: expected usage.invalid before dispatch for oversized escaped env, got out=%+v err=%v", bigOut, err)
	}
	if createCalls.Load() != 0 {
		t.Fatalf("Finding 7b: oversized create was sent over the wire (%d calls)", createCalls.Load())
	}
}

func TestMCPPart2DurableCwdContextListBudgetWaitMismatchAndLostAck(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MESH_CONFIG_DIR", filepath.Join(dir, "cfg"))
	t.Setenv("MESH_STATE_DIR", filepath.Join(dir, "state"))

	hostID := "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"
	ctxID := "0123456789abcdef0123456789abcdef"
	inst := "22222222222222222222222222222222"
	otherInst := "33333333333333333333333333333333"
	host := HostRecord{ID: hostID, MeshIdentity: hostID, MachineName: "pc", Endpoint: "ws://127.0.0.1:1/control/ws"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	app := &application{dependencies: Dependencies{Now: time.Now}}

	// Finding 1: fresh mcpControls (no context cache) gets resolved cwd from the daemon receipt,
	// and replay after context update keeps the original job's durable cwd on both Target objects.
	cwdPool := newMCPHostPool(func(_ context.Context, h HostRecord) (transport.Conn, error) {
		return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
			switch req.Type {
			case protocol.TypeHostInfo:
				return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: "pc", NameRevision: 1, Account: "remote-bob"}}, nil
			case protocol.TypeExecutionCreate:
				return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionReceipt: &protocol.ExecutionReceipt{
					HostID: h.ID, SessionID: "7K3D", Instance: inst, RequestKey: req.RequestKey,
					KeyScope: "application", Kind: "pipe", Phase: "terminal", Acceptance: "durable",
					LaunchOutcome: "started", Publication: "published", Removal: "present",
					ContextID: ctxID, Cwd: "/work/original-dir",
					Result: testTerminalResult(),
				}}, nil
			case protocol.TypeExecutionLogs:
				return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionPage: &protocol.ExecutionPage{
					HostID: h.ID, SessionID: req.SessionID, Instance: req.Instance, Stream: req.Stream,
					ObservedEnd: "0", Generation: "1", Available: true, EOF: true,
				}}, nil
			default:
				return protocol.Control{Type: protocol.TypeError, ErrorCode: "usage.invalid", Message: "unexpected"}, nil
			}
		}), nil
	}, time.Now)
	defer cwdPool.Close()
	freshControls := newMCPControls(app, cwdPool, "mac1_test_cred")
	cwdOut, err := freshControls.Exec(t.Context(), mcpnative.ExecRequest{
		Context: mcpnative.ContextRef{HostID: hostID, ContextID: ctxID}, RequestKey: "k-cwd",
		Argv: []string{"/bin/true"}, Stdin: "closed", WaitMS: 0, OutputBytes: 4096,
	}, nil)
	if err != nil || cwdOut.IsError || cwdOut.Target == nil || cwdOut.Target.Cwd != "/work/original-dir" {
		t.Fatalf("Finding 1: expected Target.Cwd=/work/original-dir, got out=%+v err=%v", cwdOut, err)
	}
	execData, ok := cwdOut.Envelope.Data.(mcpnative.ExecResultData)
	if !ok || execData.Target.Cwd != "/work/original-dir" || execData.Receipt == nil || execData.Receipt.Cwd != "/work/original-dir" {
		t.Fatalf("Finding 1: expected durable cwd on ExecResultData.Target and Receipt, got %#v", cwdOut.Envelope.Data)
	}

	// Finding 2c & 2d: wait transport error and wait instance mismatch return IsError=true
	// AND preserve the accepted job binding in envelope.Data for authorized recovery.
	for _, tc := range []struct {
		name     string
		waitResp func() (protocol.Control, error)
		wantCode agentapi.Code
	}{
		{
			name:     "wait-transport-error",
			waitResp: func() (protocol.Control, error) { return protocol.Control{}, errors.New("connection dropped mid-wait") },
			wantCode: agentapi.HostUnreachable,
		},
		{
			name: "wait-instance-mismatch",
			waitResp: func() (protocol.Control, error) {
				return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionWait: "completed", ExecutionReceipt: &protocol.ExecutionReceipt{
					HostID: hostID, SessionID: "7K3D", Instance: otherInst, RequestKey: "k-wm",
					KeyScope: "application", Kind: "pipe", Phase: "terminal", Acceptance: "durable",
					LaunchOutcome: "started", Publication: "published", Removal: "present", ContextID: ctxID,
					Result: testTerminalResult(),
				}}, nil
			},
			wantCode: agentapi.SessionInstanceMismatch,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newMCPHostPool(func(_ context.Context, h HostRecord) (transport.Conn, error) {
				return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
					switch req.Type {
					case protocol.TypeHostInfo:
						return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: "pc", NameRevision: 1}}, nil
					case protocol.TypeExecutionCreate:
						return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionReceipt: &protocol.ExecutionReceipt{
							HostID: h.ID, SessionID: "7K3D", Instance: inst, RequestKey: "k-wm",
							KeyScope: "application", Kind: "pipe", Phase: "live", Acceptance: "durable",
							LaunchOutcome: "started", Publication: "published", Removal: "present", ContextID: ctxID, Cwd: "/work/dir",
						}}, nil
					case protocol.TypeExecutionWait:
						return tc.waitResp()
					default:
						return protocol.Control{Type: protocol.TypeError, ErrorCode: "usage.invalid", Message: "unexpected"}, nil
					}
				}), nil
			}, time.Now)
			defer p.Close()
			c := newMCPControls(app, p, "mac1_test_cred")
			res, err := c.Exec(t.Context(), mcpnative.ExecRequest{
				Context: mcpnative.ContextRef{HostID: hostID, ContextID: ctxID}, RequestKey: "k-wm",
				Argv: []string{"/bin/true"}, Stdin: "closed", WaitMS: 2000, OutputBytes: 4096,
			}, nil)
			if err != nil || !res.IsError || len(res.Envelope.Errors) == 0 || res.Envelope.Errors[0].Code != tc.wantCode {
				t.Fatalf("%s: expected IsError %s, got out=%+v err=%v", tc.name, tc.wantCode, res, err)
			}
			partial, ok := res.Envelope.Data.(mcpnative.ExecResultData)
			if !ok || partial.Job == "" || partial.Target.Cwd != "/work/dir" {
				t.Fatalf("%s: expected preserved job binding for recovery, got %#v", tc.name, res.Envelope.Data)
			}
		})
	}

	// Finding 6: ListContexts enforces byte budget per page, stops before fetching page 3,
	// returns NextCursor, and resumes cleanly on the next call.
	var pagesFetched atomic.Int32
	id1 := "00000000000000000000000000000001"
	id2 := "00000000000000000000000000000002"
	id3 := "00000000000000000000000000000003"
	makeCtxInfo := func(id string) protocol.ExecutionContextInfo {
		return protocol.ExecutionContextInfo{
			HostID: hostID, ContextID: id, Handle: protocol.ContextHandle(hostID, id),
			State: protocol.ContextStateOpen, Revision: 1, Cwd: "/work/" + id, Shell: protocol.ContextShellSh,
		}
	}
	budgetPool := newMCPHostPool(func(_ context.Context, h HostRecord) (transport.Conn, error) {
		return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
			if req.Type == protocol.TypeHostInfo {
				return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: "pc", NameRevision: 1}}, nil
			}
			pagesFetched.Add(1)
			cursor := ""
			if req.ExecutionContext != nil {
				cursor = req.ExecutionContext.Cursor
			}
			switch cursor {
			case "":
				return protocol.Control{Type: protocol.TypeContextResult, ContextNextCursor: id2, ContextInfos: []protocol.ExecutionContextInfo{makeCtxInfo(id1), makeCtxInfo(id2)}}, nil
			case id1:
				return protocol.Control{Type: protocol.TypeContextResult, ContextNextCursor: id2, ContextInfos: []protocol.ExecutionContextInfo{makeCtxInfo(id2)}}, nil
			case id2:
				return protocol.Control{Type: protocol.TypeContextResult, ContextInfos: []protocol.ExecutionContextInfo{makeCtxInfo(id3)}}, nil
			default:
				return protocol.Control{Type: protocol.TypeError, ErrorCode: "usage.invalid", Message: "bad cursor"}, nil
			}
		}), nil
	}, time.Now)
	defer budgetPool.Close()
	budgetControls := newMCPControls(app, budgetPool, "mac1_test_cred")
	budgetControls.contextListBudget = 450 // fits 1 context (~350B), stops before 2nd
	p1, err := budgetControls.ListContexts(t.Context(), mcpnative.ListContextsRequest{Host: hostID, Timeout: 2 * time.Second})
	if err != nil || p1.IsError {
		t.Fatalf("Finding 6 page 1: out=%+v err=%v", p1, err)
	}
	d1, ok := p1.Envelope.Data.(agentapi.ExecutionContexts)
	if !ok || len(d1.Contexts) != 1 || d1.Contexts[0].ContextID != id1 || d1.NextCursor != id1 {
		t.Fatalf("Finding 6 page 1 expected 1 item + nextCursor=%s, got %#v", id1, p1.Envelope.Data)
	}
	if pagesFetched.Load() != 1 {
		t.Fatalf("Finding 6 fetched %d daemon pages, want 1 (stopped at byte budget)", pagesFetched.Load())
	}
	budgetControls.contextListBudget = 2000 // fits remaining 2 items across pages
	p2, err := budgetControls.ListContexts(t.Context(), mcpnative.ListContextsRequest{Host: hostID, Cursor: d1.NextCursor, Timeout: 2 * time.Second})
	if err != nil || p2.IsError {
		t.Fatalf("Finding 6 page 2: out=%+v err=%v", p2, err)
	}
	d2, ok := p2.Envelope.Data.(agentapi.ExecutionContexts)
	if !ok || len(d2.Contexts) != 2 || d2.Contexts[0].ContextID != id2 || d2.Contexts[1].ContextID != id3 || d2.NextCursor != "" {
		t.Fatalf("Finding 6 page 2 expected id2+id3 with empty NextCursor, got %#v", p2.Envelope.Data)
	}

	// Finding 8a: lost execution.create acknowledgement returns request.outcome_unknown,
	// and recovery via mesh_job_receipt under the original request_key returns the single launched job.
	var launches atomic.Int32
	lostAckPool := newMCPHostPool(func(_ context.Context, h HostRecord) (transport.Conn, error) {
		return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
			switch req.Type {
			case protocol.TypeHostInfo:
				return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: "pc", NameRevision: 1}}, nil
			case protocol.TypeExecutionCreate:
				launches.Add(1)
				// Host accepted and launched, then link dropped before reply reached frontend.
				return protocol.Control{}, errors.New("transport: read encrypted Mesh frame: EOF")
			case protocol.TypeExecutionRequest:
				return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionReceipt: &protocol.ExecutionReceipt{
					HostID: h.ID, SessionID: "7K3D", Instance: inst, RequestKey: req.RequestKey,
					KeyScope: "application", Kind: "pipe", Phase: "terminal", Acceptance: "durable",
					LaunchOutcome: "started", Publication: "published", Removal: "present",
					ContextID: ctxID, Cwd: "/work/original-dir",
					Result: testTerminalResult(),
				}}, nil
			default:
				return protocol.Control{Type: protocol.TypeError, ErrorCode: "usage.invalid", Message: "unexpected"}, nil
			}
		}), nil
	}, time.Now)
	defer lostAckPool.Close()
	lostAckControls := newMCPControls(app, lostAckPool, "mac1_test_cred")
	lostOut, err := lostAckControls.Exec(t.Context(), mcpnative.ExecRequest{
		Context: mcpnative.ContextRef{HostID: hostID, ContextID: ctxID}, RequestKey: "k-lost-ack",
		Argv: []string{"/bin/true"}, Stdin: "closed", WaitMS: 1000, OutputBytes: 4096,
	}, nil)
	if err != nil || !lostOut.IsError || len(lostOut.Envelope.Errors) == 0 || lostOut.Envelope.Errors[0].Code != agentapi.RequestOutcomeUnknown {
		t.Fatalf("Finding 8a: expected request.outcome_unknown on lost create ack, got out=%+v err=%v", lostOut, err)
	}
	recOut, err := lostAckControls.JobReceipt(t.Context(), mcpnative.JobReceiptRequest{
		Host: hostID, Context: &mcpnative.ContextRef{HostID: hostID, ContextID: ctxID}, RequestKey: "k-lost-ack", Timeout: 2 * time.Second,
	})
	if err != nil || recOut.IsError || recOut.Target == nil || recOut.Target.Cwd != "/work/original-dir" {
		t.Fatalf("Finding 8a: recovery via JobReceipt failed: out=%+v err=%v", recOut, err)
	}
	if launches.Load() != 1 {
		t.Fatalf("Finding 8a: expected 1 launch, got %d", launches.Load())
	}
}

func TestMCPExecAlreadyTerminalCreateSetsWaitCompleted(t *testing.T) {
	prepareCommandEnvironment(t)
	hostID := "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"
	ctxID := "fedcba9876543210fedcba9876543210"
	inst := "0123456789abcdef0123456789abcdef"
	if err := SaveHost(HostRecord{
		ID: hostID, MeshIdentity: hostID, MachineName: "pc", NameRevision: 1, Endpoint: "ws://127.0.0.1:1/control/ws",
	}); err != nil {
		t.Fatal(err)
	}
	app := &application{dependencies: Dependencies{Now: time.Now}}

	var waitCalls atomic.Int32
	pool := newMCPHostPool(func(_ context.Context, h HostRecord) (transport.Conn, error) {
		return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
			switch req.Type {
			case protocol.TypeHostInfo:
				return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: h.ID, MeshIdentity: h.MeshIdentity, MachineName: "pc", NameRevision: 1}}, nil
			case protocol.TypeExecutionCreate:
				return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionReceipt: &protocol.ExecutionReceipt{
					HostID: h.ID, SessionID: "7K3D", Instance: inst, RequestKey: req.RequestKey,
					KeyScope: "application", Kind: "pipe", Phase: "terminal", Acceptance: "durable",
					LaunchOutcome: "started", Publication: "published", Removal: "present",
					ContextID: ctxID, Cwd: "/tmp", Result: testTerminalResult(),
				}}, nil
			case protocol.TypeExecutionWait:
				waitCalls.Add(1)
				return protocol.Control{Type: protocol.TypeError, ErrorCode: "internal", Message: "unexpected wait call"}, nil
			case protocol.TypeExecutionLogs:
				return protocol.Control{Type: protocol.TypeExecutionReceipt, ExecutionPage: &protocol.ExecutionPage{
					HostID: h.ID, SessionID: "7K3D", Instance: inst, Stream: req.Stream,
					StartCursor: "0", EndCursor: "0", NextCursor: "0", EarliestCursor: "0", ObservedEnd: "0",
					Available: true, EOF: true, StreamClosed: true, Loss: "none",
				}}, nil
			default:
				return protocol.Control{Type: protocol.TypeError, ErrorCode: "usage.invalid", Message: "unexpected"}, nil
			}
		}), nil
	}, time.Now)
	defer pool.Close()

	controls := newMCPControls(app, pool, "mac1_test_cred")

	// When wait_ms > 0 and execution.create already returns a terminal receipt, wait must be "completed".
	out, err := controls.Exec(t.Context(), mcpnative.ExecRequest{
		Context: mcpnative.ContextRef{HostID: hostID, ContextID: ctxID}, RequestKey: "k-fast-1",
		Argv: []string{"/bin/true"}, Stdin: "closed", WaitMS: 10000, OutputBytes: 4096,
	}, nil)
	if err != nil || out.IsError {
		t.Fatalf("Exec fast-terminal failed: out=%+v err=%v", out, err)
	}
	data, ok := out.Envelope.Data.(mcpnative.ExecResultData)
	if !ok || data.State != "exited" || data.Wait != "completed" {
		t.Fatalf("Exec fast-terminal data = %#v, want state=exited wait=completed", out.Envelope.Data)
	}

	// When wait_ms == 0, wait stays omitted even for an already-terminal receipt.
	noWaitOut, err := controls.Exec(t.Context(), mcpnative.ExecRequest{
		Context: mcpnative.ContextRef{HostID: hostID, ContextID: ctxID}, RequestKey: "k-fast-0",
		Argv: []string{"/bin/true"}, Stdin: "closed", WaitMS: 0, OutputBytes: 4096,
	}, nil)
	if err != nil || noWaitOut.IsError {
		t.Fatalf("Exec wait_ms=0 failed: out=%+v err=%v", noWaitOut, err)
	}
	noWaitData, ok := noWaitOut.Envelope.Data.(mcpnative.ExecResultData)
	if !ok || noWaitData.State != "exited" || noWaitData.Wait != "" {
		t.Fatalf("Exec wait_ms=0 data = %#v, want state=exited wait=''", noWaitOut.Envelope.Data)
	}
	if waitCalls.Load() != 0 {
		t.Fatalf("expected 0 TypeExecutionWait calls for already-terminal create receipts, got %d", waitCalls.Load())
	}
}
