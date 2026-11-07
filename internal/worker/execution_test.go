package worker

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/testenv"
)

func TestNativeKeyedWorkerPublishesTerminalAndReleasesLifetimeLast(t *testing.T) {
	root := testenv.SocketTempDir(t)
	ref, err := Reserve(filepath.Join(root, "s"), session.Execution)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := execution.Open(root, BootID(), "native-test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = journal.Close() }()
	key := execution.Key{Host: "host", Class: "local-account", Principal: "account", Opaque: "native"}
	intent, err := execution.Normalize(execution.Intent{Kind: "pty", Command: []string{"/bin/sh", "-c", "exit 7"}, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	env := ExecutionEnvironment(os.Environ(), key.Host, ref, intent.Term)
	r, owner, err := journal.Reserve(t.Context(), key, intent, func() (execution.Binding, error) { return execution.NewBinding(key, intent, ref, env), nil })
	if err != nil || !owner {
		t.Fatal(err)
	}
	if err := journal.Dispatch(t.Context(), r.Binding, intent); err != nil {
		t.Fatal(err)
	}
	code, err := Run(Config{ID: ref.ID, HostID: key.Host, Dir: filepath.Join(root, "s", ref.ID), Reservation: &ref, Execution: &r.Binding, Command: intent.Command, Cwd: root, Env: env, Cols: intent.Cols, Rows: intent.Rows, Label: "agent request " + ref.Instance})
	if err != nil || code != 7 {
		t.Fatalf("native worker code=%d: %v", code, err)
	}
	if _, err := os.Stat(paths.Socket(filepath.Join(root, "s", ref.ID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker returned before socket cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "s", ref.ID, "recovery.json")); err != nil {
		t.Fatalf("short keyed PTY did not retain recovery: %v", err)
	}
	terminal, err := journal.Lookup(t.Context(), key)
	if err != nil || terminal.Phase != "terminal" || terminal.Result.Termination.Code == nil || *terminal.Result.Termination.Code != 7 {
		t.Fatalf("terminal=%+v error=%v", terminal, err)
	}
	if terminal.RetentionReason != "recovery_pinned" {
		t.Fatalf("saved recovery pin not reported: %+v", terminal)
	}
	if err := journal.RetireSession(t.Context(), filepath.Join(root, "s"), ref.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := execution.StartAttempt(t.Context(), root, r.Binding, intent, env); err == nil {
		t.Fatal("late worker entered retired attempt")
	}
}
func TestReservationRejectsEmptyMarkerPurposeAndInstanceMismatch(t *testing.T) {
	root := t.TempDir()
	ref, err := Reserve(root, session.Generic)
	if err != nil {
		t.Fatal(err)
	}
	changed := ref
	changed.Instance = "11111111111111111111111111111111"
	if err := validateReservation(root, changed); err == nil {
		t.Fatal("accepted another instance")
	}
	changed = ref
	changed.Purpose = session.Execution
	if err := validateReservation(root, changed); err == nil {
		t.Fatal("accepted another purpose")
	}
	if err := os.WriteFile(paths.Launching(filepath.Join(root, ref.ID)), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateReservation(root, ref); err == nil {
		t.Fatal("accepted tokenless marker")
	}
}
func TestOnlyRecoveryPurposeCanRenewAcrossKnownBoot(t *testing.T) {
	root := t.TempDir()
	ref, err := Reserve(root, session.Recovery)
	if err != nil {
		t.Fatal(err)
	}
	ref.BootID = "previous-boot"
	files, err := execution.OpenFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	for _, name := range []string{".reservation", ".launching"} {
		if _, err := files.Publish(filepath.Join(ref.ID, name), ref, false); err != nil {
			t.Fatal(err)
		}
	}
	next, err := RenewRecovery(root, ref)
	if err != nil {
		t.Fatal(err)
	}
	if next.Instance != ref.Instance || next.ID != ref.ID || next.BootID != BootID() {
		t.Fatalf("renewal changed attempt: %+v", next)
	}
	if err := validateReservation(root, next); err != nil {
		t.Fatal(err)
	}
	ref.Purpose = session.Execution
	if _, err := RenewRecovery(root, ref); err == nil {
		t.Fatal("keyed execution gained reboot redispatch")
	}
}

func TestExecutionClientShutdownJoinsSilentHandlers(t *testing.T) {
	dir := testenv.SocketTempDir(t)
	listener, err := net.Listen("unix", filepath.Join(dir, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	w := &Worker{executionClients: newExecutionClients()}
	w.executionClients.serve(w, server)
	done := make(chan struct{})
	close(done)
	if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	w.stopExecutionServing(listener, done)
	var data [1]byte
	if _, err := client.Read(data[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("silent handler retained socket after lifetime shutdown: %v", err)
	}
	w.executionClients.mu.Lock()
	remaining := len(w.executionClients.connections)
	w.executionClients.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d handlers still own paths", remaining)
	}
}
