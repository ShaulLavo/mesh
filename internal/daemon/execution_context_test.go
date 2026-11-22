package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func contextLifecycle(t *testing.T) *lifecycle {
	t.Helper()
	l, err := newLifecycle(lifecycleConfig{Catalog: &lifecycleTestCatalog{}, Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) { return nil, errors.New("unused") }), Host: storage.Host{ID: "host", MeshIdentity: "host"}, SessionsDir: filepath.Join(t.TempDir(), "s"), Env: []string{"PATH=/bin"}, Launch: func(cfg worker.LaunchConfig) (worker.Launched, error) {
		return worker.Launched{Meta: worker.Meta{Reservation: cfg.Reservation, ID: cfg.Reservation.ID}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.closeExecution)
	return l
}

func TestContextListPagesWithinTheFrameCap(t *testing.T) {
	l := contextLifecycle(t)
	ctx := context.WithValue(t.Context(), localClientKey{}, true)
	caller, _ := testCaller(t)
	env := map[string]string{}
	for i := 0; i < 128; i++ {
		env[fmt.Sprintf("N%03d%s", i, strings.Repeat("x", 240))] = ""
	}
	dir := t.TempDir()
	// The listing caller holds its full 64; another caller's contexts sit
	// between them in ID order and must never appear.
	other, _ := testCaller(t)
	for i := range 70 {
		owner := caller
		if i >= 64 {
			owner = other
		}
		open := protocol.Control{Type: protocol.TypeContextOpen, RequestID: "open", RequestKey: fmt.Sprintf("c-%d", i), Caller: owner, ExecutionContext: &protocol.ExecutionContextRequest{Open: &protocol.ContextSpec{Cwd: dir, Shell: protocol.ContextShellSh, Env: env}}}
		if _, err := l.contextControl(ctx, open); err != nil {
			t.Fatal(err)
		}
	}
	seen, pages, cursor := map[string]bool{}, 0, ""
	for pages < 10 {
		pages++
		reply, err := l.contextControl(ctx, protocol.Control{Type: protocol.TypeContextList, RequestID: "list", Caller: caller, ExecutionContext: &protocol.ExecutionContextRequest{Cursor: cursor}})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := reply.Encode()
		if err != nil || len(encoded) > protocol.MaxPayload/2 {
			t.Fatalf("page of %d bytes exceeds half the frame cap: %v", len(encoded), err)
		}
		for _, info := range reply.ContextInfos {
			seen[info.ContextID] = true
		}
		if reply.ContextNextCursor == "" {
			break
		}
		cursor = reply.ContextNextCursor
	}
	if len(seen) != 64 || pages < 2 {
		t.Fatalf("%d contexts over %d pages", len(seen), pages)
	}
}

func TestContextOpenRetryIgnoresHostChanges(t *testing.T) {
	l := contextLifecycle(t)
	ctx := context.WithValue(t.Context(), localClientKey{}, true)
	caller, _ := testCaller(t)
	dir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	open := protocol.Control{Type: protocol.TypeContextOpen, RequestID: "open", RequestKey: "ctx", Caller: caller, ExecutionContext: &protocol.ExecutionContextRequest{Open: &protocol.ContextSpec{Cwd: dir}}}
	first, err := l.contextControl(ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", "/bin/a-different-login-shell")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	again, err := l.contextControl(ctx, open)
	if err != nil || again.ContextInfo.ContextID != first.ContextInfo.ContextID || again.ContextInfo.ShellPath != first.ContextInfo.ShellPath {
		t.Fatalf("retry after host changes = %+v, %v", again.ContextInfo, err)
	}
}

func TestStrayCallerRejectedOnTerminalAndWatchPaths(t *testing.T) {
	caller, _ := testCaller(t)
	relay := newClientRelay(newRelayTestConn(), newRelayTestConnector(), defaultWorkerOperationTimeout)
	t.Cleanup(func() { _ = relay.Close() })
	attach := controlFrame(t, protocol.Control{Type: protocol.TypeAttach, SessionID: mustRelaySessionID(t, "7K3D").String(), Cols: 80, Rows: 24, Caller: caller})
	handled, err := relay.HandleFrame(t.Context(), attach)
	if !handled || applicationErrorCode(err) != executionUsageInvalidCode {
		t.Fatalf("attach with a credential = %v, %v", handled, err)
	}
	server := &clientServer{}
	list := controlFrame(t, protocol.Control{Type: protocol.TypeList, RequestID: "watch-list", Caller: caller})
	if reply := server.stateRequest(t.Context(), list); reply.ErrorCode != executionUsageInvalidCode {
		t.Fatalf("watch list with a credential = %+v", reply)
	}
}
