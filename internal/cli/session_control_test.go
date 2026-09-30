package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func setWorkerProbe(t *testing.T, probe func(string) error) {
	t.Helper()
	previous := probeWorker
	probeWorker = probe
	t.Cleanup(func() { probeWorker = previous })
}

func controlTestListener(t *testing.T, socket string) <-chan protocol.Control {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	received := make(chan protocol.Control, 8)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			frame, err := protocol.NewReader(conn).ReadFrame()
			if err == nil {
				request, decodeErr := protocol.DecodeControl(frame.Payload)
				if decodeErr == nil {
					received <- request
					_ = protocol.NewWriter(conn).WriteControlMsg(protocol.Control{
						Type: protocol.TypeOK, RequestID: request.RequestID, SessionID: request.SessionID,
					})
				}
			}
			_ = conn.Close()
		}
	}()
	return received
}

func TestRemoveRefusesAndKeepsALiveLocalSession(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "L1VE", worker.StateRunning)
	dir, err := paths.SessionDir("L1VE")
	if err != nil {
		t.Fatal(err)
	}
	controlTestListener(t, paths.Socket(dir))
	_, _, err = executeCommand(t, Dependencies{}, "rm", "L1VE")
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("live rm = %v, want refusal", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("live directory lost: %v", err)
	}
}

func TestRemoveDoesNotDeleteOnAMissedLivenessProbe(t *testing.T) {
	for _, state := range []string{worker.StateRunning, worker.StateExited} {
		t.Run(state, func(t *testing.T) {
			setupCommandTestHost(t)
			writeLocalSessionDir(t, "PR0B", state)
			setWorkerProbe(t, func(string) error { return context.DeadlineExceeded })
			stdout, _, err := executeCommand(t, Dependencies{}, "rm", "PR0B")
			if err == nil || strings.Contains(stdout, "removed") {
				t.Errorf("unknown liveness removal = %q, %v, want refusal", stdout, err)
			}
			dir, pathErr := paths.SessionDir("PR0B")
			if pathErr != nil {
				t.Fatal(pathErr)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("directory lost after inconclusive probe: %v", err)
			}
			if _, err := os.Stat(paths.Forgotten(dir)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("unknown session marked forgotten: %v", err)
			}
		})
	}
}

type controlTestProbe struct{}

func (controlTestProbe) Probe(context.Context, string) error { return syscall.ENOENT }

func TestLocalRemoveRetiresDurableCatalog(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "7K3D", worker.StateExited)
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	root, err := paths.SessionsDir()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := daemon.NewCatalog(daemon.CatalogConfig{
		SessionsDir: root, Store: store, Probe: controlTestProbe{}, BootID: worker.BootID,
		Host: storage.Host{ID: "local-test", MeshIdentity: "test-key", LastSeenAt: commandTestTime},
		Now:  func() time.Time { return commandTestTime.Add(time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := catalog.List(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("seeded catalog = %v, %v", rows, err)
	}
	if _, _, err := executeCommand(t, Dependencies{}, "rm", "7K3D"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err = catalog.List(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("removed session remains in durable catalog: %v, %v", rows, err)
	}
	rowsLocal, err := List()
	if err != nil || len(rowsLocal) != 0 {
		t.Fatalf("removed session remains in local catalog: %v, %v", rowsLocal, err)
	}
}

func TestSignalNormalisesName(t *testing.T) {
	const term = "term"
	for _, test := range []struct{ name, want string }{
		{"TERM", term}, {term, term}, {"SIGTERM", term}, {"SigHup", "hup"},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupCommandTestHost(t)
			writeLocalSessionDir(t, "L1VE", worker.StateRunning)
			dir, err := paths.SessionDir("L1VE")
			if err != nil {
				t.Fatal(err)
			}
			received := controlTestListener(t, paths.Socket(dir))
			setWorkerProbe(t, func(string) error { return nil })
			if _, _, err := executeCommand(t, Dependencies{}, "sig", "L1VE", test.name); err != nil {
				t.Fatal(err)
			}
			select {
			case request := <-received:
				if request.Type != protocol.TypeSignal || request.Signal != test.want {
					t.Fatalf("signal request = %+v, want %s", request, test.want)
				}
			case <-time.After(time.Second):
				t.Fatal("signal did not reach worker")
			}
		})
	}
}

func TestSignalRejectsUnknownBeforeLocalProbe(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "L1VE", worker.StateRunning)
	dir, err := paths.SessionDir("L1VE")
	if err != nil {
		t.Fatal(err)
	}
	received := controlTestListener(t, paths.Socket(dir))
	probes := 0
	setWorkerProbe(t, func(string) error { probes++; return nil })
	stdout, _, err := executeCommand(t, Dependencies{}, "sig", "L1VE", "TREM")
	if err == nil || !strings.Contains(err.Error(), "TREM") || strings.Contains(stdout, "sent") || probes != 0 {
		t.Fatalf("invalid signal = %q, %v, probes %d", stdout, err, probes)
	}
	select {
	case request := <-received:
		t.Fatalf("invalid signal delivered: %+v", request)
	default:
	}
}

func TestSignalRejectsUnknownBeforeRemoteDial(t *testing.T) {
	host := setupCommandTestHost(t)
	dials := 0
	stdout, _, err := executeCommand(t, Dependencies{DialHost: func(ctx context.Context, record HostRecord) (transport.Conn, error) {
		dials++
		return host.dial(ctx, record)
	}}, "sig", "7K3D", "bogus")
	if err == nil || dials != 0 || strings.Contains(stdout, "sent") {
		t.Fatalf("invalid remote signal = %q, %v, dials %d", stdout, err, dials)
	}
}

func TestListPreservesStateOnAnUnknownProbe(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "PR0B", worker.StateRunning)
	setWorkerProbe(t, func(string) error { return context.DeadlineExceeded })
	current, err := Find("PR0B")
	if err != nil || current.State() != worker.StateRunning {
		t.Fatalf("unknown probe state = %q, %v", current.State(), err)
	}
}

func TestListRereadsExitedMetadataAfterDefinitiveProbe(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "PR0B", worker.StateRunning)
	setWorkerProbe(t, func(string) error {
		writeLocalSessionDir(t, "PR0B", worker.StateExited)
		return syscall.ENOENT
	})
	current, err := Find("PR0B")
	if err != nil || current.State() != worker.StateExited {
		t.Fatalf("clean exit during probe state = %q, %v", current.State(), err)
	}
}

func TestAwaitHibernatedDoesNotEndOnAnUnknownProbe(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "PR0B", worker.StateRunning)
	calls := 0
	setWorkerProbe(t, func(string) error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded
		}
		return syscall.ECONNREFUSED
	})
	app := &application{}
	settled, err := app.awaitHibernated(context.Background(), resolvedSession{local: &Session{Meta: worker.Meta{ID: "PR0B"}}})
	if err != nil || calls < 2 || settled.local == nil || settled.local.State() != worker.StateInterrupted {
		t.Fatalf("hibernation settled on unknown probe: calls %d, session %+v, error %v", calls, settled.local, err)
	}
}

func TestSignalNormalisesRemoteName(t *testing.T) {
	const term = "term"
	for _, name := range []string{"TERM", term, "SIGTERM"} {
		t.Run(name, func(t *testing.T) {
			host := setupCommandTestHost(t)
			stdout, _, err := executeCommand(t, Dependencies{DialHost: host.dial}, "sig", "7K3D", name)
			if err != nil {
				t.Fatal(err)
			}
			request := host.actedOn()
			if request.Type != protocol.TypeSignal || request.Signal != term || !strings.Contains(stdout, "sent "+term) {
				t.Fatalf("remote signal = %+v, output %q", request, stdout)
			}
		})
	}
}

func TestUnsupportedSignalNamesComeFromWorkerTable(t *testing.T) {
	_, err := normaliseSignalName("bogus")
	if err == nil {
		t.Fatal("unsupported signal was accepted")
	}
	want := "use one of " + strings.Join(worker.SignalNames(), ", ")
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("unsupported signal error = %q, want %q", err, want)
	}
}

func TestUnknownProbeDoesNotAutoRecoverOnAttach(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "PR0B", worker.StateRunning)
	dir, err := paths.SessionDir("PR0B")
	if err != nil {
		t.Fatal(err)
	}
	received := controlTestListener(t, paths.Socket(dir))
	setWorkerProbe(t, func(string) error { return context.DeadlineExceeded })
	_, stderr, err := executeCommand(t, Dependencies{}, "PR0B", "--raw")
	if err != nil || strings.Contains(stderr, "recovering") {
		t.Fatalf("unknown probe attach = %v, stderr %q", err, stderr)
	}
	select {
	case request := <-received:
		if request.Type != protocol.TypeAttach || request.SessionID != "PR0B" {
			t.Fatalf("unknown probe worker request = %+v, want attach to PR0B", request)
		}
	default:
		t.Fatal("unknown probe attachment did not reach the worker")
	}
	root, err := paths.SessionsDir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unknown probe created a replacement: %v, %v", entries, err)
	}
}

func TestControlErrorOnUnknownProbePreservesWorkerFailure(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "PR0B", worker.StateRunning)
	setWorkerProbe(t, func(string) error { return context.DeadlineExceeded })
	_, _, err := executeCommand(t, Dependencies{}, "kill", "PR0B")
	if err == nil || strings.Contains(err.Error(), "already interrupted") || !strings.Contains(err.Error(), "PR0B") || !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("worker dial error = %v, want the wrapped socket failure", err)
	}
}

func TestRemoveOfflineRequiresAFreshDefinitiveProbe(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "PR0B", worker.StateRunning)
	calls := 0
	setWorkerProbe(t, func(string) error {
		calls++
		if calls < 3 {
			return syscall.ENOENT
		}
		return context.DeadlineExceeded
	})
	_, _, err := executeCommand(t, Dependencies{}, "rm", "PR0B")
	if err == nil || calls < 3 {
		t.Fatalf("offline removal without fresh proof: probes %d, error %v", calls, err)
	}
	dir, err := paths.SessionDir("PR0B")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.Forgotten(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown worker was marked forgotten: %v", err)
	}
}

func TestLatestDoesNotSkipAnUnknownLiveSession(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "PR0B", worker.StateDetached)
	setWorkerProbe(t, func(string) error { return context.DeadlineExceeded })
	current, err := Latest()
	if err != nil || current.ID != "PR0B" {
		t.Fatalf("latest skipped a potentially live session: %+v, %v", current, err)
	}
}

func TestLogsAttemptsTheWorkerOnAnUnknownProbe(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "PR0B", worker.StateRunning)
	setWorkerProbe(t, func(string) error { return context.DeadlineExceeded })
	stdout, _, err := executeCommand(t, Dependencies{}, "logs", "PR0B")
	if !errors.Is(err, syscall.ENOENT) || stdout != "" {
		t.Fatalf("unknown probe logs read stale disk output: %q, %v", stdout, err)
	}
}

func TestOfflineRemoveHidesSessionFromCLIAndPickerBeforeReconciliation(t *testing.T) {
	host := setupCommandTestHost(t)
	writeLocalSessionDir(t, "L0CL", worker.StateExited)
	dependencies := Dependencies{DialHost: host.dial}
	if _, _, err := executeCommand(t, dependencies, "rm", "L0CL"); err != nil {
		t.Fatal(err)
	}
	dir, err := paths.SessionDir("L0CL")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.Forgotten(dir)); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := executeCommand(t, dependencies, "ls", "--all")
	if err != nil || strings.Contains(stdout, "L0CL") {
		t.Fatalf("offline removed session remained in ls: %q, %v", stdout, err)
	}
	picker, err := localPickerCatalog()
	if err != nil || len(picker.Sessions) != 0 {
		t.Fatalf("offline removed session remained in picker: %+v, %v", picker, err)
	}
}
