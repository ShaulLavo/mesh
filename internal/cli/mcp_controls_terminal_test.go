package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	appspkg "github.com/shaul/mesh/internal/apps"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/transport"
)

const testTerminalHost = "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE"

// A frontend in passive discovery mode has no application identity. Its
// requests would carry whole-account authority, so terminal tools and every
// service or lifecycle mutation refuse before reaching any host. The pool is
// nil: reaching it would panic.
func TestIdentitylessFrontendRefusesTerminalAndMutations(t *testing.T) {
	c := &mcpControls{}
	job := mcpnative.JobRef{HostID: testTerminalHost, SessionID: "7K3D", Instance: "0123456789abcdef0123456789abcdef"}
	offset := uint64(0)
	route := mcpnative.ServiceRouteRequest{Host: "pc", Name: "/dev", Timeout: time.Second}
	lifecycle := mcpnative.SessionLifecycleRequest{Host: "pc", SessionID: "7K3D", Action: "shell", Timeout: time.Second}
	ctx := context.Background()
	calls := map[string]func() (mcpnative.CallOutcome, error){
		"terminal open": func() (mcpnative.CallOutcome, error) {
			return c.TerminalOpen(ctx, mcpnative.TerminalOpenRequest{Context: mcpnative.ContextRef{HostID: testTerminalHost, ContextID: "fedcba9876543210fedcba9876543210"}, RequestKey: "t", Argv: []string{"/bin/sh"}, Timeout: time.Second})
		},
		"terminal screen": func() (mcpnative.CallOutcome, error) {
			return c.TerminalScreen(ctx, mcpnative.TerminalScreenRequest{Job: job, Cols: 80, Rows: 24, Timeout: time.Second})
		},
		"terminal resize": func() (mcpnative.CallOutcome, error) {
			return c.TerminalResize(ctx, mcpnative.TerminalResizeRequest{Job: job, Cols: 80, Rows: 24, Timeout: time.Second})
		},
		"terminal input": func() (mcpnative.CallOutcome, error) {
			return c.JobInput(ctx, mcpnative.JobInputRequest{Job: job, WriteKey: "k", Offset: &offset, Data: []byte("x"), Kind: "pty", Timeout: time.Second})
		},
		"service start":     func() (mcpnative.CallOutcome, error) { return c.StartService(ctx, route) },
		"service stop":      func() (mcpnative.CallOutcome, error) { return c.StopService(ctx, route) },
		"service unpublish": func() (mcpnative.CallOutcome, error) { return c.UnpublishService(ctx, route) },
		"service publish": func() (mcpnative.CallOutcome, error) {
			return c.PublishService(ctx, mcpnative.PublishServiceRequest{Host: "pc", Target: "3000", Timeout: time.Second})
		},
		"session hibernate": func() (mcpnative.CallOutcome, error) { return c.HibernateSession(ctx, lifecycle) },
		"session recover":   func() (mcpnative.CallOutcome, error) { return c.RecoverSession(ctx, lifecycle) },
	}
	for name, call := range calls {
		out, err := call()
		if err != nil || !out.IsError || len(out.Envelope.Errors) != 1 || out.Envelope.Errors[0].Code != agentapi.UsageInvalid {
			t.Errorf("%s without identity = %+v, %v; want usage.invalid before any host", name, out.Envelope, err)
		}
	}
}

// A recovery retry that finds the recorded replacement succeeds; a reply
// naming the source itself (a live session) never does.
func TestRecoveredReplacementAcceptsRetryAndRefusesSource(t *testing.T) {
	host := HostRecord{ID: testTerminalHost, MachineName: "pc"}
	req := mcpnative.SessionLifecycleRequest{SessionID: "7K3D", Action: "shell"}
	retry := protocol.Control{Type: protocol.TypeRecovered, SessionID: "9QXA", RecoveryResult: &recovery.Result{SessionID: "9QXA", RecoveredFrom: "7K3D", Existing: true}}
	got, err := recoveredReplacement(host, req, retry)
	if err != nil || got.SessionID != "9QXA" || got.RecoveredFrom != "7K3D" {
		t.Fatalf("retry with recorded replacement = %+v, %v", got, err)
	}
	live := protocol.Control{Type: protocol.TypeRecovered, SessionID: "7K3D", RecoveryResult: &recovery.Result{SessionID: "7K3D", Existing: true, State: "detached"}}
	if _, err := recoveredReplacement(host, req, live); err == nil {
		t.Fatal("a live source was accepted as its own replacement")
	}
}

// Publishing checks the host's private name on the connection that sends
// the upsert, before it is sent. A changed name sends no upsert at all.
func TestPublishChecksPrivateNameBeforeUpsert(t *testing.T) {
	host := HostRecord{ID: testTerminalHost, MeshIdentity: testTerminalHost, MachineName: "pc", NameRevision: 1, Endpoint: "ws://127.0.0.1:1/control/ws"}
	for _, tc := range []struct {
		current string
		upserts int32
	}{{current: "changed.example", upserts: 0}, {current: "site.example", upserts: 1}} {
		var upserts atomic.Int32
		dial := func(context.Context, HostRecord) (transport.Conn, error) {
			return newPoolFixtureConn(func(req protocol.Control) (protocol.Control, error) {
				switch req.Type {
				case protocol.TypeHostInfo:
					return protocol.Control{Type: protocol.TypeHostInfoResult, Host: &protocol.HostInfo{ID: host.ID, MeshIdentity: host.MeshIdentity, MachineName: "pc", NameRevision: 1, PrivateName: tc.current}}, nil
				case protocol.TypeServiceUpsert:
					upserts.Add(1)
					return protocol.Control{Type: protocol.TypeServiceUpserted}, nil
				default:
					return protocol.Control{Type: protocol.TypeError, Message: "unexpected"}, nil
				}
			}), nil
		}
		pool := newMCPHostPool(dial, time.Now)
		c := &mcpControls{pool: pool}
		var info protocol.HostInfo
		expected := "site.example"
		_, _, err := c.poolServiceExchange(host, &info)(t.Context(), protocol.Control{Type: protocol.TypeServiceUpsert}, &expected)
		pool.Close()
		if upserts.Load() != tc.upserts || (tc.upserts == 0) != (err != nil) {
			t.Fatalf("current name %q: upserts=%d err=%v", tc.current, upserts.Load(), err)
		}
	}
}

// A failed read of the host's routes is reported as that failure. Only a
// listing that succeeds and lacks the route is service.not_found, and only
// bad options are usage.invalid.
func TestServiceLookupFailuresKeepTheirCause(t *testing.T) {
	host := HostRecord{ID: testTerminalHost, MachineName: "pc"}
	unreachable := errors.New("read service.list response: connection reset")
	failing := func(context.Context, protocol.Control, *string) (protocol.Control, protocol.HostInfo, error) {
		return protocol.Control{}, protocol.HostInfo{}, unreachable
	}
	if _, err := unpublishVia(t.Context(), host, failing, "/dev"); !errors.Is(err, unreachable) {
		t.Fatalf("unpublish with a failed lookup = %v, want the lookup error", err)
	}
	flags, err := publishFlags(mcpnative.PublishServiceRequest{Target: "3000", Route: "/dev"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publishVia(t.Context(), host, failing, "3000", flags); !errors.Is(err, unreachable) {
		t.Fatalf("publish with a failed lookup = %v, want the lookup error", err)
	}
	empty := func(context.Context, protocol.Control, *string) (protocol.Control, protocol.HostInfo, error) {
		return protocol.Control{Type: protocol.TypeServiceListed, Services: []protocol.ServiceInfo{}}, protocol.HostInfo{}, nil
	}
	_, err = unpublishVia(t.Context(), host, empty, "/dev")
	var failure *agentapi.Failure
	if !errors.As(err, &failure) || failure.Err.Code != agentapi.ServiceNotFound {
		t.Fatalf("unpublish of an absent route = %v, want service.not_found", err)
	}
}

// A failed start never hands an agent the route's saved command or its last
// output, which can carry credentials; the diagnostic stays with the owner.
func TestServiceStartFailureKeepsOwnerDiagnostic(t *testing.T) {
	host := HostRecord{ID: testTerminalHost, MachineName: "pc"}
	diagnostic := `route /dev did not start: exited (session 7K3D, command "API_TOKEN=SECRET-IN-COMMAND ./serve")` + "\n\nlast output:\nkey=SECRET-IN-OUTPUT\n" + strings.Repeat("x", 4096)
	refusing := func(_ context.Context, request protocol.Control, _ *string) (protocol.Control, protocol.HostInfo, error) {
		return protocol.Control{Type: protocol.TypeError, RequestID: request.RequestID, Message: diagnostic}, protocol.HostInfo{}, nil
	}
	_, err := startStopServiceVia(t.Context(), host, refusing, protocol.TypeServiceStart, "service start", "dev")
	failure := boundedAgentError(ownerOnlyDiagnostic(err, "services.start", host, "/dev"), "services.start", host.ID)
	if strings.Contains(failure.Message, "SECRET") || len(failure.Message) > 512 || !strings.Contains(failure.Message, "/dev") {
		t.Fatalf("start failure shown to the agent = %q", failure.Message)
	}
	if failure.Code == agentapi.HostUnreachable {
		t.Fatalf("a host refusal read as unreachable: %+v", failure)
	}
}

// silentCreateConn answers host.info and accepts a create it never answers,
// like a host that is slow to launch. Closing it ends the wait.
type silentCreateConn struct {
	host    HostRecord
	replies chan protocol.Frame
	closed  chan struct{}
	once    sync.Once
}

func (c *silentCreateConn) WriteFrame(frame protocol.Frame) error {
	req, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("decode fixture control: %w", err)
	}
	if req.Type != protocol.TypeHostInfo {
		return nil
	}
	reply := protocol.Control{Type: protocol.TypeHostInfoResult, RequestID: req.RequestID, Host: &protocol.HostInfo{ID: c.host.ID, MeshIdentity: c.host.MeshIdentity, MachineName: "pc", NameRevision: 1, ExecutionContextSupported: true}}
	payload, err := reply.Encode()
	if err != nil {
		return fmt.Errorf("encode fixture reply: %w", err)
	}
	c.replies <- protocol.Frame{Kind: protocol.KindControl, Payload: payload}
	return nil
}

func (c *silentCreateConn) ReadFrame() (protocol.Frame, error) {
	select {
	case frame := <-c.replies:
		return frame, nil
	case <-c.closed:
		return protocol.Frame{}, io.EOF
	}
}

func (c *silentCreateConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// mesh_terminal_open answers within the caller's deadline. A host that has
// not replied by then leaves the outcome to the request key.
func TestTerminalOpenHonoursShortDeadline(t *testing.T) {
	prepareCommandEnvironment(t)
	host := HostRecord{ID: testTerminalHost, MeshIdentity: testTerminalHost, MachineName: "pc", NameRevision: 1, Endpoint: "ws://127.0.0.1:1/control/ws"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	dial := func(context.Context, HostRecord) (transport.Conn, error) {
		return &silentCreateConn{host: host, replies: make(chan protocol.Frame, 2), closed: make(chan struct{})}, nil
	}
	pool := newMCPHostPool(dial, time.Now)
	t.Cleanup(pool.Close)
	c := newMCPControls(&application{dependencies: Dependencies{Now: time.Now}}, pool, "mac1_test_cred")
	started := time.Now()
	out, err := c.TerminalOpen(t.Context(), mcpnative.TerminalOpenRequest{Context: mcpnative.ContextRef{HostID: testTerminalHost, ContextID: "fedcba9876543210fedcba9876543210"}, RequestKey: "t1", Argv: []string{"/bin/sh"}, Cols: 80, Rows: 24, Term: "xterm-256color", Timeout: 300 * time.Millisecond})
	elapsed := time.Since(started)
	if err != nil || !out.IsError || out.Envelope.Errors[0].Code != agentapi.RequestOutcomeUnknown {
		t.Fatalf("unanswered open = %+v, %v; want request.outcome_unknown", out.Envelope, err)
	}
	t.Logf("open returned after %s with a 300ms deadline", elapsed)
	if elapsed > 10*time.Second {
		t.Fatalf("open took %s, ignoring its deadline", elapsed)
	}
}

// App listings count against the per-host budget of short controls, and a
// successful listing is bounded.
func TestAppListingsShareTheHostBudget(t *testing.T) {
	prepareCommandEnvironment(t)
	host := HostRecord{ID: testTerminalHost, MeshIdentity: testTerminalHost, MachineName: "pc", NameRevision: 1, Endpoint: "ws://127.0.0.1:1/control/ws"}
	if err := SaveHost(host); err != nil {
		t.Fatal(err)
	}
	var running, peak atomic.Int32
	release := make(chan struct{})
	records := make([]appspkg.Record, 250)
	for i := range records {
		records[i] = appspkg.Record{ID: "abcd", Status: "ready"}
	}
	app := &application{dependencies: Dependencies{Now: time.Now, AppRequest: func(context.Context, string, appspkg.Request) (appspkg.Result, error) {
		now := running.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		<-release
		running.Add(-1)
		return appspkg.Result{Apps: records}, nil
	}}}
	pool := newMCPHostPool(nil, time.Now)
	t.Cleanup(pool.Close)
	c := newMCPControls(app, pool, "")
	var calls sync.WaitGroup
	outcomes := make(chan mcpnative.CallOutcome, 12)
	for range 12 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			out, err := c.ListApps(context.Background(), mcpnative.ListAppsRequest{Host: testTerminalHost, Timeout: 30 * time.Second})
			if err != nil {
				t.Error(err)
			}
			outcomes <- out
		}()
	}
	deadline := time.Now().Add(30 * time.Second)
	for running.Load() < 8 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	calls.Wait()
	close(outcomes)
	if peak.Load() > 8 {
		t.Fatalf("%d concurrent app listings on one host, budget is 8", peak.Load())
	}
	for out := range outcomes {
		data, ok := out.Envelope.Data.(agentapi.AppsData)
		if out.IsError || !ok || len(data.Apps) != agentapi.MaxAppRows || !data.Truncated {
			t.Fatalf("listing = %+v", out.Envelope)
		}
	}
}
