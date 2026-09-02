package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

type lifecycleTestCatalog struct {
	mu             sync.Mutex
	sessions       []storage.Session
	reconcileErr   error
	reconcileHook  func(context.Context) error
	reconcileCalls int
}

func (c *lifecycleTestCatalog) Reconcile(ctx context.Context) error {
	c.mu.Lock()
	c.reconcileCalls++
	hook := c.reconcileHook
	err := c.reconcileErr
	c.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return err
}

func (c *lifecycleTestCatalog) reconciliationCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reconcileCalls
}

func (c *lifecycleTestCatalog) List(context.Context) ([]storage.Session, error) {
	return append([]storage.Session(nil), c.sessions...), nil
}

func (c *lifecycleTestCatalog) Get(_ context.Context, id storage.SessionID) (storage.Session, error) {
	for _, item := range c.sessions {
		if item.ID == id {
			return item, nil
		}
	}
	return storage.Session{}, errors.New("not found")
}

type lifecycleConnectorFunc func(context.Context, protocol.SessionID) (transport.Conn, error)

func (f lifecycleConnectorFunc) ConnectWorker(ctx context.Context, id protocol.SessionID) (transport.Conn, error) {
	return f(ctx, id)
}

func TestLifecycleCreatesAndPublishesDetachedWorker(t *testing.T) {
	catalog := &lifecycleTestCatalog{}
	var launched worker.LaunchConfig
	lifecycle, err := newLifecycle(lifecycleConfig{
		Catalog:     catalog,
		Connector:   lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) { return nil, errors.New("unused") }),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: "/state/s",
		Executable:  "/opt/mesh",
		Env:         []string{"TERM=xterm-256color"},
		Launch: func(cfg worker.LaunchConfig) (worker.Launched, error) {
			launched = cfg
			return worker.Launched{Meta: worker.Meta{ID: "7K3D"}, Dir: "/state/s/7K3D"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	response, handled, err := lifecycle.HandleControl(context.Background(), protocol.Control{
		Type:      protocol.TypeCreate,
		RequestID: "create-1",
		Command:   []string{"sh", "-lc", "printf ready"},
		Cwd:       "/work",
		Cols:      120,
		Rows:      40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("create was not handled")
	}
	if response.Type != protocol.TypeCreated || response.RequestID != "create-1" || response.SessionID != "7K3D" {
		t.Fatalf("create response = %+v", response)
	}
	wantLaunch := worker.LaunchConfig{
		SessionsDir: "/state/s",
		Executable:  "/opt/mesh",
		Command:     []string{"sh", "-lc", "printf ready"},
		Cwd:         "/work",
		Env:         []string{"TERM=xterm-256color"},
		Cols:        120,
		Rows:        40,
	}
	if !reflect.DeepEqual(launched, wantLaunch) {
		t.Fatalf("launch config = %#v, want %#v", launched, wantLaunch)
	}
	if got := catalog.reconciliationCount(); got != 1 {
		t.Fatalf("reconciliations = %d, want 1", got)
	}
}

func TestLifecycleListsSessionsAndHostIdentity(t *testing.T) {
	createdAt := time.Date(2026, time.August, 29, 1, 2, 3, 0, time.UTC)
	attachedAt := createdAt.Add(time.Minute)
	exitCode := 4
	catalog := &lifecycleTestCatalog{sessions: []storage.Session{{
		ID:                 "7K3D",
		HostID:             "host-a",
		Command:            []string{"sh"},
		Cwd:                "/work",
		State:              storage.StateExited,
		CreatedAt:          createdAt,
		LastAttachedAt:     &attachedAt,
		ExitCode:           &exitCode,
		LastOutputSequence: 99,
	}}}
	tailscaleName := "desktop.example.ts.net"
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog:   catalog,
		Connector: failingLifecycleConnector(),
		Host: storage.Host{
			ID:            "host-a",
			MeshIdentity:  "mesh-key",
			TailscaleName: &tailscaleName,
			LastSeenAt:    createdAt,
		},
		SessionsDir: "/state/s",
	})

	response, handled, err := lifecycle.HandleControl(context.Background(), protocol.Control{Type: protocol.TypeList, RequestID: "list-1"})
	if err != nil || !handled {
		t.Fatalf("list handled = %v, error = %v", handled, err)
	}
	if response.Type != protocol.TypeListed || response.RequestID != "list-1" || len(response.Sessions) != 1 {
		t.Fatalf("list response = %+v", response)
	}
	got := response.Sessions[0]
	if got.ID != "7K3D" || got.HostID != "host-a" || got.State != string(storage.StateExited) || got.LastOutputSequence != 99 || got.ExitCode == nil || *got.ExitCode != 4 {
		t.Fatalf("session info = %+v", got)
	}

	response, handled, err = lifecycle.HandleControl(context.Background(), protocol.Control{Type: protocol.TypeHostInfo, RequestID: "host-1"})
	if err != nil || !handled {
		t.Fatalf("host info handled = %v, error = %v", handled, err)
	}
	if response.Type != protocol.TypeHostInfoResult || response.RequestID != "host-1" || response.Host == nil || response.Host.ID != "host-a" || response.Host.MeshIdentity != "mesh-key" || response.Host.TailscaleName != tailscaleName {
		t.Fatalf("host response = %+v", response)
	}
}

func TestLifecycleForwardsOneShotControls(t *testing.T) {
	for _, controlType := range []string{protocol.TypeSignal, protocol.TypeKill, protocol.TypeLogs} {
		t.Run(controlType, func(t *testing.T) {
			workerConn := &lifecycleRecordingConn{}
			if controlType == protocol.TypeKill {
				workerConn.readFrame = controlFrame(t, protocol.Control{
					Type:      protocol.TypeOK,
					RequestID: "control-1",
					SessionID: "7K3D",
				})
			} else if controlType == protocol.TypeLogs {
				workerConn.readFrame = controlFrame(t, protocol.Control{
					Type:      protocol.TypeLogged,
					RequestID: "control-1",
					SessionID: "7K3D",
					Output:    []byte("recent output"),
				})
			}
			catalog := &lifecycleTestCatalog{}
			if controlType == protocol.TypeLogs {
				catalog.sessions = []storage.Session{{ID: "7K3D", State: storage.StateRunning}}
			}
			lifecycle := mustLifecycle(t, lifecycleConfig{
				Catalog: catalog,
				Connector: lifecycleConnectorFunc(func(_ context.Context, id protocol.SessionID) (transport.Conn, error) {
					if id.String() != "7K3D" {
						t.Fatalf("worker ID = %q", id.String())
					}
					return workerConn, nil
				}),
				Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
				SessionsDir: "/state/s",
			})

			response, handled, err := lifecycle.HandleControl(context.Background(), protocol.Control{
				Type:      controlType,
				RequestID: "control-1",
				SessionID: "7K3D",
				Signal:    "term",
				Tail:      4096,
			})
			if err != nil || !handled {
				t.Fatalf("control handled = %v, error = %v", handled, err)
			}
			wantType := protocol.TypeOK
			if controlType == protocol.TypeLogs {
				wantType = protocol.TypeLogged
			}
			if response.Type != wantType || response.RequestID != "control-1" || response.SessionID != "7K3D" {
				t.Fatalf("control response = %+v", response)
			}
			if controlType == protocol.TypeLogs && !bytes.Equal(response.Output, []byte("recent output")) {
				t.Fatalf("logs output = %q", response.Output)
			}
			if !workerConn.closed || len(workerConn.frames) != 1 {
				t.Fatalf("worker connection closed = %v, frames = %d", workerConn.closed, len(workerConn.frames))
			}
			if (controlType == protocol.TypeKill || controlType == protocol.TypeLogs) && !workerConn.read {
				t.Fatalf("%s completed without reading the worker response", controlType)
			}
			forwarded, err := protocol.DecodeControl(workerConn.frames[0].Payload)
			if err != nil {
				t.Fatal(err)
			}
			wantSignal := ""
			if controlType == protocol.TypeSignal {
				wantSignal = "term"
			}
			wantTail := 0
			if controlType == protocol.TypeLogs {
				wantTail = 4096
			}
			if forwarded.Type != controlType || forwarded.SessionID != "7K3D" || forwarded.Signal != wantSignal || forwarded.Tail != wantTail {
				t.Fatalf("forwarded control = %+v", forwarded)
			}
		})
	}
}

func TestLifecycleReadsExitedSessionLogsWithoutConnectingWorker(t *testing.T) {
	sessionsDir := t.TempDir()
	sessionDir := filepath.Join(sessionsDir, "7K3D")
	if err := os.Mkdir(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "worker.log"), []byte("old diagnostic\nlast line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	connectCalls := 0
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog: &lifecycleTestCatalog{sessions: []storage.Session{{ID: "7K3D", State: storage.StateExited}}},
		Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
			connectCalls++
			return nil, errors.New("exited worker must not be contacted")
		}),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: sessionsDir,
	})

	response, handled, err := lifecycle.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeLogs, RequestID: "logs-exited", SessionID: "7K3D", Tail: 10,
	})
	if err != nil || !handled {
		t.Fatalf("logs handled = %v, error = %v", handled, err)
	}
	if response.Type != protocol.TypeLogged || string(response.Output) != "last line\n" {
		t.Fatalf("logs response = %+v", response)
	}
	if connectCalls != 0 {
		t.Fatalf("worker connection calls = %d, want 0", connectCalls)
	}
}

func TestLifecycleRetriesPublicationWithoutLaunchingDuplicate(t *testing.T) {
	wantPublishErr := errors.New("temporary catalog failure")
	var publishAttempts atomic.Int32
	catalog := &lifecycleTestCatalog{reconcileHook: func(context.Context) error {
		if publishAttempts.Add(1) == 1 {
			return wantPublishErr
		}
		return nil
	}}
	var launchCalls atomic.Int32
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog:     catalog,
		Connector:   failingLifecycleConnector(),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: "/state/s",
		Launch: func(worker.LaunchConfig) (worker.Launched, error) {
			launchCalls.Add(1)
			return worker.Launched{Meta: worker.Meta{ID: "7K3D"}}, nil
		},
	})
	request := protocol.Control{
		Type:      protocol.TypeCreate,
		RequestID: "stable-create-request",
		Command:   []string{"sh", "-lc", "do-once"},
		Cwd:       "/work",
	}

	if _, _, err := lifecycle.HandleControl(context.Background(), request); !errors.Is(err, wantPublishErr) {
		t.Fatalf("first create error = %v, want publication failure", err)
	}
	response, handled, err := lifecycle.HandleControl(context.Background(), request)
	if err != nil || !handled {
		t.Fatalf("retried create handled = %v, error = %v", handled, err)
	}
	if response.Type != protocol.TypeCreated || response.SessionID != "7K3D" {
		t.Fatalf("retried response = %+v", response)
	}
	if got := launchCalls.Load(); got != 1 {
		t.Fatalf("worker launches = %d, want 1", got)
	}
	if got := publishAttempts.Load(); got != 2 {
		t.Fatalf("publication attempts = %d, want 2", got)
	}

	changed := request
	changed.Command = []string{"sh", "-lc", "different-side-effect"}
	if _, handled, err := lifecycle.HandleControl(context.Background(), changed); !handled || err == nil {
		t.Fatalf("reused request ID handled = %v, error = %v; want handled error", handled, err)
	}
	if got := launchCalls.Load(); got != 1 {
		t.Fatalf("worker launches after conflicting retry = %d, want 1", got)
	}
}

func TestLifecycleCoalescesConcurrentCreateRequest(t *testing.T) {
	catalog := &lifecycleTestCatalog{}
	launchStarted := make(chan struct{})
	releaseLaunch := make(chan struct{})
	var startedOnce sync.Once
	var launchCalls atomic.Int32
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog:     catalog,
		Connector:   failingLifecycleConnector(),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: "/state/s",
		Launch: func(worker.LaunchConfig) (worker.Launched, error) {
			launchCalls.Add(1)
			startedOnce.Do(func() { close(launchStarted) })
			<-releaseLaunch
			return worker.Launched{Meta: worker.Meta{ID: "7K3D"}}, nil
		},
	})
	request := protocol.Control{Type: protocol.TypeCreate, RequestID: "same-request", Command: []string{"sh"}}
	type result struct {
		response protocol.Control
		err      error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			response, _, err := lifecycle.HandleControl(context.Background(), request)
			results <- result{response: response, err: err}
		}()
	}
	select {
	case <-launchStarted:
	case <-time.After(time.Second):
		t.Fatal("worker launch did not start")
	}
	close(releaseLaunch)
	for range 2 {
		got := <-results
		if got.err != nil || got.response.SessionID != "7K3D" {
			t.Fatalf("concurrent create = %+v, %v", got.response, got.err)
		}
	}
	if got := launchCalls.Load(); got != 1 {
		t.Fatalf("concurrent worker launches = %d, want 1", got)
	}
	if got := catalog.reconciliationCount(); got != 1 {
		t.Fatalf("concurrent reconciliations = %d, want 1", got)
	}
}

func TestLifecycleBoundsPublicationByDaemonContext(t *testing.T) {
	catalog := &lifecycleTestCatalog{reconcileHook: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Context:        context.Background(),
		Catalog:        catalog,
		Connector:      failingLifecycleConnector(),
		Host:           storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir:    "/state/s",
		PublishTimeout: 20 * time.Millisecond,
		Launch: func(worker.LaunchConfig) (worker.Launched, error) {
			return worker.Launched{Meta: worker.Meta{ID: "7K3D"}}, nil
		},
	})
	started := time.Now()
	_, _, err := lifecycle.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeCreate, RequestID: "bounded-publish", Command: []string{"sh"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("publication error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded publication took %v", elapsed)
	}
}

func TestLifecycleRejectsMalformedRequestsBeforeSideEffects(t *testing.T) {
	launchCalls := 0
	connectCalls := 0
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog: &lifecycleTestCatalog{},
		Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
			connectCalls++
			return nil, errors.New("unexpected connect")
		}),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: "/state/s",
		Launch: func(worker.LaunchConfig) (worker.Launched, error) {
			launchCalls++
			return worker.Launched{}, errors.New("unexpected launch")
		},
	})

	requests := []protocol.Control{
		{Type: protocol.TypeCreate, RequestID: "request-1", Command: []string{""}},
		{Type: protocol.TypeSignal, RequestID: "request-2", SessionID: "7K3D", Signal: "bogus"},
		{Type: protocol.TypeKill, RequestID: "request-3", SessionID: "../X"},
		{Type: protocol.TypeLogs, RequestID: "request-4", SessionID: "7K3D", Tail: protocol.MaxLogTail + 1},
		{Type: protocol.TypeList},
	}
	for _, request := range requests {
		if _, handled, err := lifecycle.HandleControl(context.Background(), request); !handled || err == nil {
			t.Errorf("request %+v handled = %v, error = %v; want handled error", request, handled, err)
		}
	}
	if launchCalls != 0 || connectCalls != 0 {
		t.Fatalf("invalid requests launched %d workers and connected %d times", launchCalls, connectCalls)
	}
	if _, handled, err := lifecycle.HandleControl(context.Background(), protocol.Control{Type: protocol.TypeAttach}); handled || err != nil {
		t.Fatalf("attach handled = %v, error = %v; want relay-owned", handled, err)
	}
	if _, handled, err := lifecycle.HandleControl(nil, protocol.Control{Type: protocol.TypeList, RequestID: "request-4"}); !handled || err == nil { //nolint:staticcheck // boundary test intentionally passes a nil context
		t.Fatalf("nil-context list handled = %v, error = %v; want handled error", handled, err)
	}
}

func mustLifecycle(t *testing.T, cfg lifecycleConfig) *lifecycle {
	t.Helper()
	got, err := newLifecycle(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func failingLifecycleConnector() WorkerConnector {
	return lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
		return nil, errors.New("unexpected worker connection")
	})
}

type lifecycleRecordingConn struct {
	readFrame protocol.Frame
	read      bool
	frames    []protocol.Frame
	closed    bool
}

func (c *lifecycleRecordingConn) ReadFrame() (protocol.Frame, error) {
	c.read = true
	if c.readFrame.Kind == 0 {
		return protocol.Frame{}, errors.New("unexpected read")
	}
	return c.readFrame, nil
}

func (c *lifecycleRecordingConn) WriteFrame(frame protocol.Frame) error {
	frame.Payload = append([]byte(nil), frame.Payload...)
	c.frames = append(c.frames, frame)
	return nil
}

func (c *lifecycleRecordingConn) Close() error {
	c.closed = true
	return nil
}

func TestCreateWithoutACommandUsesTheHostShell(t *testing.T) {
	var launched worker.LaunchConfig
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog: &lifecycleTestCatalog{},
		Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
			return nil, errors.New("unexpected connect")
		}),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: "/state/s",
		Launch: func(config worker.LaunchConfig) (worker.Launched, error) {
			launched = config
			return worker.Launched{}, errors.New("stop after the command is chosen")
		},
	})

	// A client on a Mac sends no command rather than /bin/zsh, which names a
	// path that need not exist on this host.
	_, _, _ = lifecycle.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeCreate, RequestID: "request-shell", Cols: 80, Rows: 24,
	})
	if len(launched.Command) == 0 {
		t.Fatal("no command reached the worker")
	}
	if launched.Command[0] != hostShell() {
		t.Fatalf("command = %q, want the host shell %q", launched.Command, hostShell())
	}
}

func TestLogsFallsBackWhenTheWorkerIsAlreadyGone(t *testing.T) {
	t.Parallel()

	// A session that exited moments ago still reads as running until
	// reconciliation notices, and its socket is already gone. Its output is on
	// disk, so logs must serve that rather than report a dial failure.
	sessionsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sessionsDir, "7K3D"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "7K3D", "worker.log"), []byte("output on disk\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog: &lifecycleTestCatalog{sessions: []storage.Session{{ID: "7K3D", State: storage.StateRunning}}},
		Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
			return nil, fmt.Errorf("dial unix worker sock: %w", syscall.ENOENT)
		}),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: sessionsDir,
	})

	response, handled, err := lifecycle.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeLogs, RequestID: "logs-1", SessionID: "7K3D", Tail: 4096,
	})
	if err != nil || !handled {
		t.Fatalf("logs handled = %v, error = %v", handled, err)
	}
	if !bytes.Contains(response.Output, []byte("output on disk")) {
		t.Fatalf("logs output = %q, want the durable tail", response.Output)
	}
}

func TestLogsStillReportsARealForwardingFailure(t *testing.T) {
	t.Parallel()

	// Only a missing worker falls through. A live worker that fails for another
	// reason must not be papered over with a stale log.
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog: &lifecycleTestCatalog{sessions: []storage.Session{{ID: "7K3D", State: storage.StateRunning}}},
		Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
			return nil, errors.New("worker refused the control frame")
		}),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: t.TempDir(),
	})

	if _, _, err := lifecycle.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeLogs, RequestID: "logs-2", SessionID: "7K3D", Tail: 4096,
	}); err == nil {
		t.Fatal("a real forwarding failure was swallowed")
	}
}
