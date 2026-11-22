package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session/execution"
)

// A journal that cannot be locked in time is busy, not a sign that the caller
// lost its job. Found by the native MCP streaming benchmark under load, where
// a job's own owner was told access.denied while tailing its output.
func TestBoundExecutionLockTimeoutIsNotADenial(t *testing.T) {
	f := newScopedFixture(t.Context(), t, false)
	sessions := filepath.Join(compactSocketTempDir(t), "s")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	l := f.server.lifecycle
	l.sessionsDir = sessions
	alice, aliceID := testCaller(t)
	bob, _ := testCaller(t)
	key := execution.Key{Host: string(l.host.ID), Class: execution.ApplicationClass, Principal: string(aliceID), Opaque: "term-1", Operation: execution.OperationExec}
	id := reserveTerminalJob(t, l, key)
	instance := reservedInstance(t, sessions, id)
	local := context.WithValue(t.Context(), localClientKey{}, true)
	control := func(ctx context.Context, kind string, caller protocol.CallerCredential) protocol.Control {
		request := protocol.Control{Type: kind, RequestID: "busy-" + kind, SessionID: id, Instance: instance, Caller: caller}
		response, handled, err := l.HandleControl(ctx, request)
		if !handled {
			t.Fatalf("%s not handled", kind)
		}
		if err != nil {
			return clientRequestError(request, err)
		}
		return response
	}

	release := holdJournalLock(t, filepath.Dir(sessions))
	for _, kind := range []string{protocol.TypeExecutionInspect, protocol.TypeExecutionKill} {
		busy, cancel := context.WithTimeout(local, 200*time.Millisecond)
		got := control(busy, kind, alice)
		cancel()
		if got.ErrorCode != stateUnavailableCode || got.ExecutionReceipt != nil || got.ExecutionPage != nil || got.ExecutionDelivery != "" {
			t.Fatalf("%s with a held journal lock = %+v", kind, got)
		}
	}
	release()

	if got := control(local, protocol.TypeExecutionInspect, bob); got.ErrorCode != accessDeniedCode {
		t.Fatalf("another caller after release = %+v", got)
	}
	got := control(local, protocol.TypeExecutionInspect, alice)
	if got.ErrorCode != "" || got.ExecutionReceipt == nil || got.ExecutionReceipt.KillRequested {
		t.Fatalf("owner after release = %+v (the busy kill must not have been recorded)", got)
	}
}

func reservedInstance(t *testing.T, sessions, id string) string {
	t.Helper()
	// #nosec G304 -- The test's own temporary sessions directory.
	raw, err := os.ReadFile(filepath.Join(sessions, id, ".reservation"))
	if err != nil {
		t.Fatal(err)
	}
	var reservation struct {
		Instance string `json:"instance"`
	}
	if err := json.Unmarshal(raw, &reservation); err != nil || reservation.Instance == "" {
		t.Fatalf("reservation %s: %q %v", id, raw, err)
	}
	return reservation.Instance
}

// holdJournalLock takes the execution journal's lock the way another daemon
// operation would, so every journal read waits on it.
func holdJournalLock(t *testing.T, state string) func() {
	t.Helper()
	// #nosec G304 -- The test's own temporary state directory.
	file, err := os.OpenFile(filepath.Join(state, "execution", "journal.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}
	t.Cleanup(release)
	return release
}
