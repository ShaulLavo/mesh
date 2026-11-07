package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

func enableScopedOutput(t *testing.T, f *scopedFixture) {
	t.Helper()
	policy, err := f.store.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	entry := policy.Entries[0]
	_, err = f.store.Grant(t.Context(), policy.Revision, access.GrantInput{Subject: entry.Subject, ExpiresAt: entry.ExpiresAt, Permissions: []string{"metadata", "session-output"}}, observeOtherRoles(f.state))
	if err != nil {
		t.Fatal(err)
	}
}
func scopedOutput(t *testing.T, f *scopedFixture, output protocol.AccessOutput) protocol.AccessOutputReply {
	t.Helper()
	conn, err := f.dial(t, transport.ScopedAdmission)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	raw, err := json.Marshal(protocol.AccessOutputRequest{Type: protocol.TypeAccessOutput, RequestID: "output", Output: output})
	if err != nil {
		t.Fatal(err)
	}
	request, err := protocol.DecodeAccessOutput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: raw}); err != nil {
		t.Fatal(err)
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(frame.Payload, []byte(disclosureSentinel)) {
		t.Fatalf("private record disclosed: %s", frame.Payload)
	}
	reply, err := protocol.DecodeAccessOutputReply(frame.Payload, request.Output)
	if err != nil {
		t.Fatal(err, string(frame.Payload))
	}
	return reply
}
func prepareOutputFiles(t *testing.T, f *scopedFixture, state storage.SessionState, lines []string, tail []byte) {
	t.Helper()
	root := compactSocketTempDir(t)
	f.server.lifecycle.sessionsDir = root
	f.catalog.rows[0].State = state
	dir := filepath.Join(root, "7K3D")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Log(dir), tail, 0600); err != nil {
		t.Fatal(err)
	}
	record := recovery.Record{Version: 1, HostID: f.target, SessionID: "7K3D", CheckpointAt: time.Date(2026, 11, 7, 1, 2, 3, 0, time.UTC), Shell: "/bin/sh", ShellDirectory: "/" + disclosureSentinel, DirectorySource: recovery.DirectoryShell, Command: []string{disclosureSentinel}, Title: disclosureSentinel, Lines: lines, Restart: &recovery.Command{Argv: []string{disclosureSentinel}, Cwd: "/" + disclosureSentinel}}
	if err := recovery.Write(dir, record); err != nil {
		t.Fatal(err)
	}
	connector, err := newWorkerConnector(root, f.catalog)
	if err != nil {
		t.Fatal(err)
	}
	f.server.lifecycle.connector = connector
}
func TestScopedOutputPermissionBeforeEveryPayloadRead(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	for _, output := range []protocol.AccessOutput{{SessionID: "7K3D", Format: "tail"}, {SessionID: "7K3D", Format: "preview", Cols: 1, Rows: 1}} {
		reply := scopedOutput(t, f, output)
		if reply.Result != nil || reply.ErrorCode != "access.denied" {
			t.Fatal("metadata read output", reply)
		}
	}
	if f.catalog.reads.Load() != 0 || f.workers.calls.Load() != 0 {
		t.Fatal("denied output reached owner")
	}
}
func TestScopedSavedOutputDimensionsTimesBinaryAndMissing(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	enableScopedOutput(t, f)
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = strings.Repeat("界", 80)
	}
	tail := bytes.Repeat([]byte{0, 0xff, 'x', '\n'}, protocol.MaxLogTail/4+2)
	prepareOutputFiles(t, f, storage.StateExited, lines, tail)
	for _, dims := range [][2]int{{1, 1}, {0, 0}, {160, 24}, {3, 2}} {
		assertSavedOutputPreview(t, f, dims)
	}
	for _, maximum := range []int{1, protocol.DefaultLogTail, protocol.MaxLogTail} {
		reply := scopedOutput(t, f, protocol.AccessOutput{SessionID: "7K3D", Format: "tail", MaxBytes: maximum})
		if reply.Result == nil || reply.Result.Source != "saved-tail" || reply.Result.ObservedAt == nil {
			t.Fatal("saved tail", reply)
		}
		got, err := base64.StdEncoding.DecodeString(*reply.Result.BytesBase64)
		if err != nil || !bytes.Equal(got, tail[len(tail)-maximum:]) {
			t.Fatal("saved bytes changed", len(got), err)
		}
	}
	if err := os.Remove(filepath.Join(f.server.lifecycle.sessionsDir, "7K3D", "recovery.json")); err != nil {
		t.Fatal(err)
	}
	reply := scopedOutput(t, f, protocol.AccessOutput{SessionID: "7K3D", Format: "preview"})
	if reply.Result != nil || reply.ErrorCode != "state.unavailable" {
		t.Fatal("missing screen certified live", reply)
	}
	if err := os.WriteFile(filepath.Join(f.server.lifecycle.sessionsDir, "7K3D", "recovery.json"), []byte(disclosureSentinel), 0600); err != nil {
		t.Fatal(err)
	}
	reply = scopedOutput(t, f, protocol.AccessOutput{SessionID: "7K3D", Format: "preview"})
	if reply.Result != nil || reply.ErrorCode != "state.unavailable" {
		t.Fatal("corrupt screen disclosed", reply)
	}
}
func assertSavedOutputPreview(t *testing.T, f *scopedFixture, dims [2]int) {
	t.Helper()
	reply := scopedOutput(t, f, protocol.AccessOutput{SessionID: "7K3D", Format: "preview", Cols: dims[0], Rows: dims[1]})
	if reply.Result == nil {
		t.Fatal("saved preview unavailable", reply)
	}
	result := reply.Result
	cols, rows := dims[0], dims[1]
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	if result.Source != "saved-preview" || result.ObservedAt != nil || result.CheckpointAt == nil || len(result.Preview) != rows || len(result.StyledPreview) != 0 || result.BytesBase64 != nil {
		t.Fatal("wrong saved shape", result)
	}
	for _, line := range result.Preview {
		if ansi.StringWidth(line) > cols || ansi.StringWidth(line) != cols-cols%2 {
			t.Fatalf("saved width=%d request=%d", ansi.StringWidth(line), cols)
		}
	}
}

type scopedReadWorker struct {
	ctx      context.Context
	listener net.Listener
	requests atomic.Int32
	finished chan struct{}
	respond  func(protocol.Control) protocol.Control
}

func startScopedReadWorker(t *testing.T, f *scopedFixture, respond func(protocol.Control) protocol.Control) *scopedReadWorker {
	t.Helper()
	w := &scopedReadWorker{ctx: f.ctx, respond: respond, finished: make(chan struct{})}
	t.Cleanup(func() {
		if w.listener != nil {
			_ = w.listener.Close()
		}
		select {
		case <-w.finished:
		case <-time.After(3 * time.Second):
			t.Error("read worker did not join")
		}
	})
	listener, err := net.Listen("unix", paths.Socket(filepath.Join(f.server.lifecycle.sessionsDir, "7K3D")))
	if err != nil {
		close(w.finished)
		t.Fatal(err)
	}
	w.listener = listener
	go w.serve()
	return w
}
func (w *scopedReadWorker) serve() {
	defer close(w.finished)
	for {
		stream, err := w.listener.Accept()
		if err != nil {
			return
		}
		w.answer(stream)
	}
}
func (w *scopedReadWorker) answer(stream net.Conn) {
	stop := context.AfterFunc(w.ctx, func() { _ = stream.Close() })
	defer stop()
	defer func() { _ = stream.Close() }()
	_ = stream.SetDeadline(time.Now().Add(3 * time.Second))
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		return
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return
	}
	w.requests.Add(1)
	response := w.respond(request)
	raw, err := response.Encode()
	if err == nil {
		_ = conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: raw})
	}
}
func TestScopedLiveOutputOnlySendsReadRequests(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	enableScopedOutput(t, f)
	prepareOutputFiles(t, f, storage.StateDetached, []string{"saved"}, []byte("saved-tail"))
	worker := startScopedReadWorker(t, f, func(request protocol.Control) protocol.Control {
		switch request.Type {
		case protocol.TypeLogs:
			return protocol.Control{Type: protocol.TypeLogged, RequestID: request.RequestID, SessionID: request.SessionID, Output: []byte{0, 255, 'L'}}
		case protocol.TypeInspect:
			return protocol.Control{Type: protocol.TypeInspected, RequestID: request.RequestID, SessionID: request.SessionID, Inspection: &protocol.SessionInspection{ObservedAt: time.Now().UTC(), CurrentDirectory: "/" + disclosureSentinel, DirectorySource: protocol.DirectorySourceProcess, ForegroundCommand: disclosureSentinel, TerminalTitle: disclosureSentinel, Attached: true, Preview: []string{"L"}, StyledPreview: []protocol.PreviewLine{{Runs: []protocol.PreviewRun{{Text: "L", Style: protocol.PreviewStyle{Bold: true}}}}}}}
		default:
			return protocol.Control{Type: protocol.TypeError, Message: "unexpected mutation"}
		}
	})
	tail := scopedOutput(t, f, protocol.AccessOutput{SessionID: "7K3D", Format: "tail"})
	if tail.Result == nil || tail.Result.Source != "live-tail" || *tail.Result.BytesBase64 != "AP9M" {
		t.Fatal("live tail", tail)
	}
	preview := scopedOutput(t, f, protocol.AccessOutput{SessionID: "7K3D", Format: "preview", Cols: 1, Rows: 1})
	if preview.Result == nil || preview.Result.Source != "live-preview" || len(preview.Result.StyledPreview) != 1 || !preview.Result.StyledPreview[0].Runs[0].Style.Bold {
		t.Fatal("live style projection", preview)
	}
	if worker.requests.Load() != 2 || f.launches.Load() != 0 || f.catalog.mutations.Load() != 0 {
		t.Fatal("read issued mutation or extra worker read")
	}
}
func TestScopedJustExitedTailUsesSavedSource(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	enableScopedOutput(t, f)
	prepareOutputFiles(t, f, storage.StateDetached, []string{"saved"}, []byte("durable-diagnostics"))
	reply := scopedOutput(t, f, protocol.AccessOutput{SessionID: "7K3D", Format: "tail"})
	if reply.Result == nil || reply.Result.Source != "saved-tail" {
		t.Fatal("just exited fallback", reply)
	}
}
func TestScopedOutputLegacyDenialsRemainClosed(t *testing.T) {
	for _, loose := range []bool{false, true} {
		f := newScopedFixture(t.Context(), t, loose)
		enableScopedOutput(t, f)
		assertOutputLegacyDenials(t, f)
	}
}
func assertOutputLegacyDenials(t *testing.T, f *scopedFixture) {
	t.Helper()
	for _, name := range deniedScopedTypes {
		assertScopedDenied(t, f, protocol.Frame{Kind: protocol.KindControl, Payload: []byte(`{"type":"` + name + `","requestId":"legacy","sessionId":"7K3D","cols":1,"rows":1}`)})
	}
	for parent, actions := range deniedScopedActions {
		for _, action := range actions {
			assertScopedDenied(t, f, scopedActionFrame(t, parent, action))
		}
	}
	if f.catalog.reads.Load() != 0 || f.workers.calls.Load() != 0 || f.owners.calls.Load() != 0 || f.launches.Load() != 0 {
		t.Fatal("output key reached legacy owner")
	}
}
func TestScopedOutputRechecksAfterQueuedCatalogBeforeSavedOpen(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	enableScopedOutput(t, f)
	prepareOutputFiles(t, f, storage.StateExited, []string{"saved"}, []byte("tail"))
	binding, err := f.store.Bind(access.Subject(f.subject))
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	f.catalog.entered = make(chan struct{})
	f.catalog.release = release
	done := make(chan error, 1)
	go func() {
		_, err := f.server.access.Output(f.ctx, binding, protocol.AccessOutput{SessionID: "7K3D", Format: "tail", MaxBytes: 10})
		done <- err
	}()
	select {
	case <-f.catalog.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("catalog not entered")
	}
	if _, err := f.store.Revoke(t.Context(), 2, access.Subject(f.subject)); err != nil {
		t.Fatal(err)
	}
	// Removing the payload makes any unguarded saved open observably different from access denial.
	if err := os.Remove(paths.Log(filepath.Join(f.server.lifecycle.sessionsDir, "7K3D"))); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, access.ErrDenied) {
			t.Fatal("saved open after revoke", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("output did not join")
	}
}

func TestScopedOutputQueuedSlotRechecksRevocation(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	enableScopedOutput(t, f)
	reader := f.server.access
	for range cap(reader.slots) {
		reader.slots <- struct{}{}
	}
	defer func() {
		for len(reader.slots) > 0 {
			<-reader.slots
		}
	}()
	binding, err := f.store.Bind(access.Subject(f.subject))
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	var once sync.Once
	binding = binding.Constrain(func() bool { once.Do(func() { close(entered) }); return true })
	done := make(chan error, 1)
	go func() {
		_, err := reader.Output(f.ctx, binding, protocol.AccessOutput{SessionID: "7K3D", Format: "tail", MaxBytes: 1})
		done <- err
	}()
	<-entered
	if _, err := f.store.Revoke(t.Context(), 2, access.Subject(f.subject)); err != nil {
		t.Fatal(err)
	}
	<-reader.slots
	select {
	case err := <-done:
		if !errors.Is(err, access.ErrDenied) {
			t.Fatal("queued output admitted", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued output did not join")
	}
	if f.catalog.reads.Load() != 0 || f.workers.calls.Load() != 0 {
		t.Fatal("queued output reached owner")
	}
}
func TestScopedReadRechecksAfterWorkerConnectBeforeRequest(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	enableScopedOutput(t, f)
	binding, err := f.store.Bind(access.Subject(f.subject))
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	conn, err := transport.NewStreamConn(client)
	if err != nil {
		t.Fatal(err)
	}
	f.server.lifecycle.connector = lifecycleConnectorFunc(func(ctx context.Context, _ protocol.SessionID) (transport.Conn, error) {
		_, err := f.store.Revoke(ctx, 2, access.Subject(f.subject))
		if err != nil {
			return nil, fmt.Errorf("fixture revoke: %w", err)
		}
		return conn, nil
	})
	_, err = f.server.lifecycle.forwardRead(t.Context(), protocol.Control{Type: protocol.TypeLogs, RequestID: "read", SessionID: "7K3D", Tail: 1}, func(context.Context) error { return binding.RequireOutput() })
	if !errors.Is(err, access.ErrDenied) {
		t.Fatal("request admitted after connect", err)
	}
	_ = server.SetReadDeadline(time.Now().Add(time.Second))
	var probe [1]byte
	if n, err := server.Read(probe[:]); n != 0 || err == nil {
		t.Fatal("worker request escaped after revoke", n, err)
	}
}
func TestScopedOutputCompletedAfterRevokeNeverReplies(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	enableScopedOutput(t, f)
	prepareOutputFiles(t, f, storage.StateDetached, []string{"saved"}, []byte("saved"))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	startScopedReadWorker(t, f, func(request protocol.Control) protocol.Control {
		close(entered)
		<-release
		return protocol.Control{Type: protocol.TypeLogged, RequestID: request.RequestID, SessionID: request.SessionID, Output: []byte("protected-completion")}
	})
	conn, err := f.dial(t, transport.ScopedAdmission)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	raw := []byte(`{"type":"access.output","requestId":"delayed","accessOutput":{"sessionId":"7K3D","format":"tail"}}`)
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: raw}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker read not admitted")
	}
	if _, err := f.store.Revoke(t.Context(), 2, access.Subject(f.subject)); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	done := make(chan error, 1)
	go func() { _, err := conn.ReadFrame(); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("completed revoked read disclosed bytes")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked output did not close")
	}
}
func TestScopedOutputCancellationClosesOnlyOneShotSocket(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	enableScopedOutput(t, f)
	binding, err := f.store.Bind(access.Subject(f.subject))
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	conn, err := transport.NewStreamConn(client)
	if err != nil {
		t.Fatal(err)
	}
	f.server.lifecycle.connector = lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) { return conn, nil })
	entered, joined := make(chan struct{}), make(chan error, 1)
	go func() {
		reader := protocol.NewReader(server)
		_, err := reader.ReadFrame()
		close(entered)
		if err == nil {
			_, err = reader.ReadFrame()
		}
		joined <- err
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.server.access.Output(ctx, binding, protocol.AccessOutput{SessionID: "7K3D", Format: "tail", MaxBytes: 1})
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled output succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("output did not cancel")
	}
	select {
	case err := <-joined:
		if err == nil {
			t.Fatal("read socket stayed open")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("read socket did not join")
	}
	if f.catalog.mutations.Load() != 0 || f.launches.Load() != 0 || !binding.Current() {
		t.Fatal("cancellation changed authority or session owner")
	}
}

func TestScopedMalformedOutputNeverReachesPayloadOwners(t *testing.T) {
	for _, loose := range []bool{false, true} {
		assertMalformedScopedOutput(t, loose)
	}
}
func assertMalformedScopedOutput(t *testing.T, loose bool) {
	t.Helper()
	f := newScopedFixture(t.Context(), t, loose)
	enableScopedOutput(t, f)
	valid := `{"type":"access.output","requestId":"strict","accessOutput":{"sessionId":"7K3D","format":"preview"}}`
	for _, raw := range []string{
		strings.Replace(valid, `"format":"preview"`, `"format":"preview","maxBytes":0`, 1),
		strings.Replace(valid, `"format":"preview"`, `"format":"preview","cols":1,"cols":2`, 1),
		strings.Replace(valid, `"format":"preview"`, `"format":"tail","rows":0`, 1),
		strings.Replace(valid, `"format":"preview"`, `"format":"preview","recovery":{"lines":["secret"]}`, 1),
		strings.Replace(valid, `"accessOutput":`, `"principal":"local-account","accessOutput":`, 1),
		strings.Replace(valid, `"requestId":"strict"`, `"requestId":"strict","requestId":"other"`, 1),
		strings.Replace(valid, `"type":"access.output"`, `"type":"access.output","type":"access.read"`, 1),
		"{\xff}", strings.Repeat(" ", protocol.MaximumAccessRequest) + valid,
	} {
		assertScopedDenied(t, f, protocol.Frame{Kind: protocol.KindControl, Payload: []byte(raw)})
	}
	if f.catalog.reads.Load() != 0 || f.workers.calls.Load() != 0 || f.launches.Load() != 0 {
		t.Fatal("malformed output reached payload owner")
	}
}
func TestScopedSavedPreviewWithoutScreenRemainsUnavailable(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	enableScopedOutput(t, f)
	prepareOutputFiles(t, f, storage.StateExited, nil, nil)
	output := protocol.AccessOutput{SessionID: "7K3D", Format: "preview"}
	reply := scopedOutput(t, f, output)
	if reply.Result != nil || reply.ErrorCode != "state.unavailable" {
		t.Fatal("missing screen certified saved", reply)
	}
	tail := scopedOutput(t, f, protocol.AccessOutput{SessionID: "7K3D", Format: "tail"})
	if tail.Result == nil || tail.Result.BytesBase64 == nil || *tail.Result.BytesBase64 != "" {
		t.Fatal("empty tail was not represented explicitly", tail)
	}
}
func TestWorkerReadAdmissionDoesNotInventAuthority(t *testing.T) {
	if err := workerReadAdmission(t.Context()); err == nil {
		t.Fatal("missing read admission accepted")
	}
	ctx := context.WithValue(t.Context(), readAdmissionKey{}, readAdmission(nil))
	if err := workerReadAdmission(ctx); err == nil {
		t.Fatal("nil read admission accepted")
	}
	f := newScopedFixture(t.Context(), t, false)
	_, err := f.server.lifecycle.forwardRead(t.Context(), protocol.Control{Type: protocol.TypeKill}, trustedReadAdmission)
	if err == nil || f.workers.calls.Load() != 0 {
		t.Fatal("read helper accepted mutation", err)
	}
}
