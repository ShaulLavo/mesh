package cli

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
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
