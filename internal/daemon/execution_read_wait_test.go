package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
)

// A job whose result has just appeared is confirmed under its output stream's
// lock while the receipt is refreshed, before the page is read. A logs read
// that said it will wait 30 s must wait that long for a busy stream there too,
// not fail at the host's 1.5 s default lock wait (#318).
func TestExecutionLogsWaitsForItsCallerThroughFinalConfirmation(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	sessions := filepath.Join(compactSocketTempDir(t), "s")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	l := f.server.lifecycle
	l.sessionsDir = sessions
	key := execution.Key{Host: string(l.host.ID), Class: executionDeviceClass, Principal: "device-a", Opaque: "read-wait"}
	id := reserveTerminalJob(t, l, key)
	instance := reservedInstance(t, sessions, id)
	b := finishTerminalCapture(t, l, id, instance, "confirmed output")

	release := holdStreamLock(t, filepath.Dir(sessions), b)
	// Longer than the host's default lock wait, well inside the caller's.
	time.AfterFunc(2*time.Second, release)
	local := context.WithValue(t.Context(), localClientKey{}, true)
	request := protocol.Control{Type: protocol.TypeExecutionLogs, RequestID: "read-wait", SessionID: id, Instance: instance,
		Stream: execution.KindPTY, MaxBytes: 64, TimeoutMillis: 30000}
	response, handled, err := l.HandleControl(local, request)
	if !handled || err != nil {
		t.Fatalf("logs read during final confirmation: handled=%v err=%v", handled, err)
	}
	page := response.ExecutionPage
	if page == nil || string(page.Bytes) != "confirmed output" || !page.EOF {
		t.Fatalf("logs page = %+v", page)
	}
}

// A read waiting on a busy stream ends when its local client disconnects,
// rather than staying announced until the caller's 30 s runs out.
func TestExecutionLogsStopsWhenTheLocalClientLeaves(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	sessions := filepath.Join(compactSocketTempDir(t), "s")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	l := f.server.lifecycle
	l.sessionsDir = sessions
	key := execution.Key{Host: string(l.host.ID), Class: executionDeviceClass, Principal: "device-a", Opaque: "read-left"}
	id := reserveTerminalJob(t, l, key)
	instance := reservedInstance(t, sessions, id)
	b := finishTerminalCapture(t, l, id, instance, "never read")
	holdStreamLock(t, filepath.Dir(sessions), b)

	client := newServerTestConn()
	done := make(chan error, 1)
	go func() { done <- f.server.Handle(localServerContext(t.Context()), client) }()
	client.pushRead(serverControlFrame(t, protocol.Control{Type: protocol.TypeExecutionLogs, RequestID: "left", SessionID: id, Instance: instance,
		Stream: execution.KindPTY, MaxBytes: 64, TimeoutMillis: 30000}))
	client.pushReadError(io.EOF)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the host kept reading for a client that had left")
	}
}

// finishTerminalCapture writes a job's whole output and durable result the
// way its worker would, leaving the journal to notice the result.
func finishTerminalCapture(t *testing.T, l *lifecycle, id, instance, output string) execution.Binding {
	t.Helper()
	journal, err := l.executionJournal()
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.executionOutput.Binding(id, instance, string(l.host.ID))
	if err != nil {
		t.Fatal(err)
	}
	intent, err := execution.Normalize(execution.Intent{Kind: execution.KindPTY, Command: []string{"/bin/true"}, Cwd: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Dispatch(t.Context(), b, intent); err != nil {
		t.Fatal(err)
	}
	state := filepath.Dir(l.sessionsDir)
	attempt, err := execution.StartAttempt(t.Context(), state, b, intent, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = attempt.Close() }()
	capture, err := execution.OpenCapture(state, b, execution.KindPTY)
	if err != nil {
		t.Fatal(err)
	}
	capture.Offer([]byte(output))
	stream, err := capture.Finish(t.Context(), "eof")
	if err != nil {
		t.Fatal(err)
	}
	code := 0
	result := execution.Result{Binding: b, Termination: execution.Termination{Kind: "exit", Code: &code}, Capture: "complete", Streams: []execution.StreamResult{stream}}
	if err := attempt.Finish(result); err != nil {
		t.Fatal(err)
	}
	return b
}

// holdStreamLock takes a job's output stream lock exclusively, the way a slow
// commit or settlement would.
func holdStreamLock(t *testing.T, state string, b execution.Binding) func() {
	t.Helper()
	name := filepath.Join(state, "execution", "attempts", b.Reservation.Instance, execution.KindPTY, "stream.lock")
	// #nosec G304 -- The test's own temporary state directory.
	file, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	release := func() {
		select {
		case <-done:
		default:
			close(done)
			_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
			_ = file.Close()
		}
	}
	t.Cleanup(release)
	return release
}
