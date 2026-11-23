package daemon

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/storage"
)

func TestInlineCreateReturnsResultAndOutputOnce(t *testing.T) {
	f := newNativePipeDaemon(t)
	req := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "inline", RequestKey: "inline", ExecutionKind: execution.KindPipe, Cwd: f.root, Command: []string{"/bin/sh", "-c", "printf out; printf err >&2; exit 3"}, WaitMillis: 10000, OutputBytes: 4096}
	first := f.request(t, req)
	assertInlineCompletion(t, first, 3, "out", "err")
	// A replay of the same key observes the same job; it never launches again.
	replay := f.request(t, req)
	assertInlineCompletion(t, replay, 3, "out", "err")
	if replay.ExecutionReceipt.Instance != first.ExecutionReceipt.Instance {
		t.Fatalf("replay changed instance: %s then %s", first.ExecutionReceipt.Instance, replay.ExecutionReceipt.Instance)
	}
	conflict := req
	conflict.Command = []string{"/bin/true"}
	reply, err := f.exchange(conflict)
	if err != nil || reply.Type != protocol.TypeError || reply.ErrorCode != "request.key_conflict" || len(reply.ExecutionOutput) != 0 {
		t.Fatalf("conflicting replay = %+v, %v", reply, err)
	}
}

func TestInlineCreateTimesOutWithTheLiveReceipt(t *testing.T) {
	f := newNativePipeDaemon(t)
	req := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "slow", RequestKey: "slow", ExecutionKind: execution.KindPipe, Cwd: f.root, Command: []string{"/bin/sh", "-c", "printf started; exec sleep 30"}, WaitMillis: 200, OutputBytes: 64}
	reply := f.request(t, req)
	if reply.ExecutionWait != "timed_out" || reply.ExecutionReceipt == nil || reply.ExecutionReceipt.Phase == "terminal" || reply.ExecutionReceipt.Result != nil {
		t.Fatalf("slow inline create = %+v", reply)
	}
	for _, page := range reply.ExecutionOutput {
		if page.EOF {
			t.Fatalf("live stream %s reported EOF", page.Stream)
		}
	}
	// Without waitMillis a create answers as before, with no observation.
	plain := req
	plain.RequestKey, plain.WaitMillis, plain.OutputBytes = "plain", 0, 0
	if reply := f.request(t, plain); reply.ExecutionWait != "" || reply.ExecutionOutput != nil {
		t.Fatalf("plain create carried an observation: %+v", reply)
	}
}

// A client that disconnects after sending an inline create never causes a
// second launch: a retry of its key finds the one job and its result.
func TestInlineCreateDisconnectThenRetryRunsOnce(t *testing.T) {
	f := newNativePipeDaemon(t)
	marker := filepath.Join(f.root, "launches")
	req := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "gone", RequestKey: "gone", ExecutionKind: execution.KindPipe, Cwd: f.root, Command: []string{"/bin/sh", "-c", "echo x >> " + marker + "; sleep 0.2; printf done"}, WaitMillis: 10000, OutputBytes: 64}
	conn, err := net.DialTimeout("unix", SocketPath(f.state), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewWriter(conn).WriteControlMsg(req); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	retry := f.request(t, req)
	assertInlineCompletion(t, retry, 0, "done", "")
	launches, err := os.ReadFile(marker) //nolint:gosec // the fixture's marker under its own temporary root
	if err != nil || string(launches) != "x\n" {
		t.Fatalf("launches = %q, %v; want exactly one", launches, err)
	}
}

// A device whose grant is revoked receives no observation of its job.
func TestInlineObservationNeedsACurrentGrant(t *testing.T) {
	state := compactSocketTempDir(t)
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	device, grantCtx := bindExecutionFixtureGrant(t, state, host.ID)
	l := mustLifecycle(t, lifecycleConfig{Catalog: &lifecycleTestCatalog{}, Connector: failingLifecycleConnector(),
		Host: storage.Host{ID: storage.HostID(host.ID), MeshIdentity: host.ID}, SessionsDir: filepath.Join(state, "s")})
	t.Cleanup(l.closeExecution)
	key := prepareQueuedExecution(t, l, host.ID, device, state, false)
	request := protocol.Control{Type: protocol.TypeExecutionCreate, RequestID: "observe", RequestKey: key.Opaque, WaitMillis: 1000, OutputBytes: 64}
	j, err := l.executionJournal()
	if err != nil {
		t.Fatal(err)
	}
	r, err := j.Lookup(grantCtx, key)
	if err != nil {
		t.Fatal(err)
	}
	created, _ := executionResponse(request, r, nil)
	if observed, err := l.withInlineResult(grantCtx, request, created, nil); err != nil || observed.ExecutionWait != "completed" {
		t.Fatalf("current grant observation = %+v, %v", observed, err)
	}
	if err := identity.RevokeDevice(t.Context(), state, device); err != nil {
		t.Fatal(err)
	}
	observed, _ := l.withInlineResult(grantCtx, request, created, nil)
	if observed.ExecutionWait != "" || observed.ExecutionOutput != nil {
		t.Fatalf("revoked grant received an observation: %+v", observed)
	}
}

func assertInlineCompletion(t *testing.T, reply protocol.Control, code int, stdout, stderr string) {
	t.Helper()
	r := reply.ExecutionReceipt
	if reply.ExecutionWait != "completed" || r == nil || r.Phase != "terminal" || r.Result == nil || r.Result.Termination.Code == nil || *r.Result.Termination.Code != code {
		t.Fatalf("inline completion = %+v", reply)
	}
	want := map[string]string{execution.StreamStdout: stdout, execution.StreamStderr: stderr}
	if len(reply.ExecutionOutput) != len(want) {
		t.Fatalf("inline pages = %+v", reply.ExecutionOutput)
	}
	for _, page := range reply.ExecutionOutput {
		if page.SessionID != r.SessionID || page.Instance != r.Instance || !page.EOF || !bytes.Equal(page.Bytes, []byte(want[page.Stream])) {
			t.Fatalf("inline page %s = %+v", page.Stream, page)
		}
	}
}
