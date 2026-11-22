package cli

import (
	"context"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpnative"
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
