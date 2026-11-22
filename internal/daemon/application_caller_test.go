package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/appcaller"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
)

func testCaller(t *testing.T) (protocol.CallerCredential, appcaller.ID) {
	t.Helper()
	credential, err := appcaller.Generate()
	if err != nil {
		t.Fatal(err)
	}
	id, err := appcaller.Derive(appcaller.Scope{Class: appcaller.ScopeLocalAccount, Principal: "daemon-account"}, credential)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.CallerCredential(credential.Encode()), id
}

func TestExecutionOwnerGuard(t *testing.T) {
	local := context.WithValue(t.Context(), localClientKey{}, true)
	alice, aliceID := testCaller(t)
	bob, _ := testCaller(t)
	owned := execution.Key{Host: "h", Class: execution.ApplicationClass, Principal: string(aliceID), Opaque: "k", Context: "ctx-a", Operation: execution.OperationExec}
	account := execution.Key{Host: "h", Class: "local-account", Principal: "daemon-account", Opaque: "k"}
	cases := []struct {
		name    string
		request protocol.Control
		owner   execution.Key
		allowed bool
	}{
		{"owner", protocol.Control{Caller: alice}, owned, true},
		{"owner in its context", protocol.Control{Caller: alice, ExecutionContext: &protocol.ExecutionContextRequest{ContextID: "ctx-a"}}, owned, true},
		{"owner through another context", protocol.Control{Caller: alice, ExecutionContext: &protocol.ExecutionContextRequest{ContextID: "ctx-b"}}, owned, false},
		{"other caller", protocol.Control{Caller: bob}, owned, false},
		{"caller claiming administration", protocol.Control{Caller: bob, Administration: true}, owned, false},
		{"no credential, no administration", protocol.Control{}, owned, false},
		{"explicit administration", protocol.Control{Administration: true}, owned, true},
		{"account job, account caller", protocol.Control{}, account, true},
		{"account job, application caller", protocol.Control{Caller: alice}, account, false},
		{"malformed credential", protocol.Control{Caller: "mac1_bad"}, owned, false},
	}
	for _, tc := range cases {
		err := authorizeExecutionOwner(local, tc.request, tc.owner)
		if (err == nil) != tc.allowed {
			t.Errorf("%s: err=%v, allowed=%v", tc.name, err, tc.allowed)
		}
	}
	if err := authorizeExecutionOwner(t.Context(), protocol.Control{Caller: alice}, owned); !errors.Is(err, access.ErrDenied) {
		t.Fatalf("unauthenticated connection = %v", err)
	}
}

// A journal that cannot be locked in time is busy, not a sign that the caller
// lost its job. Found by the native MCP streaming benchmark under load, where
// a job's own owner was told access.denied while tailing its output.
func TestBoundExecutionLockTimeoutIsNotADenial(t *testing.T) {
	journal, err := execution.Open(t.TempDir(), "boot", "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	alice, _ := testCaller(t)
	local := context.WithValue(t.Context(), localClientKey{}, true)
	expired, cancel := context.WithTimeout(local, 0)
	defer cancel()
	b := execution.Binding{RequestHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	requests := []protocol.Control{
		{Caller: alice},
		{Caller: alice, ExecutionContext: &protocol.ExecutionContextRequest{ContextID: "ctx-a"}},
		{},
	}
	for i, request := range requests {
		err := (&lifecycle{}).authorizeBoundExecution(expired, request, journal, b)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, access.ErrDenied) {
			t.Fatalf("lock timeout for request %d = %v", i, err)
		}
		if !journalBusy(err) {
			t.Fatalf("lock timeout not recognized as busy: %v", err)
		}
		// boundExecutionControl sends a busy journal as a typed execution failure.
		_, wire := executionResponse(protocol.Control{}, execution.Receipt{}, executionBoundaryError("execution control", err))
		if code := clientRequestError(protocol.Control{}, wire).ErrorCode; code != stateUnavailableCode {
			t.Fatalf("lock timeout wire code = %s", code)
		}
	}
	// A receipt that does not exist still reads as a denial to an application.
	if err := (&lifecycle{}).authorizeBoundExecution(local, protocol.Control{Caller: alice}, journal, b); !errors.Is(err, access.ErrDenied) {
		t.Fatalf("missing receipt = %v", err)
	}
}

func TestStrayCallerAndErrorCodes(t *testing.T) {
	alice, _ := testCaller(t)
	if err := rejectStrayCaller(protocol.Control{Type: protocol.TypeList, Caller: alice}); applicationErrorCode(err) != executionUsageInvalidCode {
		t.Fatalf("stray caller on session.list = %v", err)
	}
	for _, kind := range []string{protocol.TypeContextOpen, protocol.TypeExecutionLogs} {
		if err := rejectStrayCaller(protocol.Control{Type: kind, Caller: alice}); err != nil {
			t.Fatalf("%s rejected its caller: %v", kind, err)
		}
	}
	for err, code := range map[error]string{
		errCallerDenied:                                "access.denied",
		execution.ErrContextNotFound:                   "context.not_found",
		execution.ErrContextClosed:                     "context.closed",
		execution.ErrContextConflict:                   "context.conflict",
		execution.ErrContextLimit:                      resourceLimitCode,
		contextUsage("x"):                              executionUsageInvalidCode,
		&callerValidationError{err: errCallerRequired}: executionUsageInvalidCode,
	} {
		if got := executionCode(executionBoundaryError("op", err)); got != code {
			t.Errorf("%v -> %s, want %s", err, got, code)
		}
	}
	formatted := protocol.Control{Caller: alice}
	if text := formatted.Caller.String(); text == string(alice) {
		t.Fatal("credential printed in a formatted control")
	}
}

func TestContextJobResolution(t *testing.T) {
	c := execution.Context{Cwd: "/work", Shell: protocol.ContextShellLogin, ShellPath: "/bin/zsh", Env: map[string]string{"A": "1", "B": "2"}}
	job := contextJobIntent{Kind: execution.KindPipe, ShellCommand: "make", Cwd: "sub", Env: map[string]string{"B": "3"}, Stdin: execution.StdinClosed}
	intent, env, err := resolveContextJob(c, job)
	if err != nil || intent.Cwd != "/work/sub" || len(intent.Command) != 4 || intent.Command[0] != "/bin/zsh" || intent.Command[1] != "-l" || env["A"] != "1" || env["B"] != "3" {
		t.Fatalf("login resolution = %+v %v %v", intent, env, err)
	}
	c.Shell = protocol.ContextShellNone
	if _, _, err := resolveContextJob(c, job); err == nil {
		t.Fatal("shell none accepted a command string")
	}
	job.ShellCommand, job.Command, job.Cwd = "", []string{"/bin/true"}, "/abs"
	if intent, _, err := resolveContextJob(c, job); err != nil || intent.Cwd != "/abs" {
		t.Fatalf("argv resolution = %+v %v", intent, err)
	}
	merged := withEnvOverlay([]string{"PATH=/bin", "A=old"}, map[string]string{"A": "new"})
	if len(merged) != 2 || merged[0] != "PATH=/bin" || merged[1] != "A=new" {
		t.Fatalf("overlay = %v", merged)
	}
	for _, name := range []string{"", "MESH_SESSION", "1A", "A-B"} {
		if validContextEnvName(name) == nil {
			t.Errorf("accepted env name %q", name)
		}
	}
}
