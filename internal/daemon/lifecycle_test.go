package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

type lifecycleTestCatalog struct {
	mu             sync.Mutex
	sessions       []storage.Session
	getHook        func()
	getErr         error
	getCalls       int
	reconcileErr   error
	reconcileHook  func(context.Context) error
	reconcileCalls int
	retired        int
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
	c.mu.Lock()
	defer c.mu.Unlock()
	c.getCalls++
	if c.getHook != nil {
		c.getHook()
	}
	if c.getErr != nil {
		return storage.Session{}, c.getErr
	}
	for _, item := range c.sessions {
		if item.ID == id {
			return item, nil
		}
	}
	return storage.Session{}, sql.ErrNoRows
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
		HostID:      "host-a",
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
	if response.Type != protocol.TypeHostInfoResult || response.RequestID != "host-1" || response.Host == nil || response.Host.ID != "host-a" || response.Host.MeshIdentity != "mesh-key" || response.Host.TailscaleName != tailscaleName || !response.Host.ServiceHealthSupported {
		t.Fatalf("host response = %+v", response)
	}
}

func TestLifecycleForwardsOneShotControls(t *testing.T) {
	for _, controlType := range []string{protocol.TypeSignal, protocol.TypeKill, protocol.TypeLogs, protocol.TypeInspect} {
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
			} else if controlType == protocol.TypeInspect {
				observedAt := time.Date(2026, time.September, 4, 9, 0, 0, 0, time.UTC)
				lastOutputAt := observedAt.Add(-time.Second)
				workerConn.readFrame = controlFrame(t, protocol.Control{
					Type:      protocol.TypeInspected,
					RequestID: "control-1",
					SessionID: "7K3D",
					Inspection: &protocol.SessionInspection{
						ObservedAt:        observedAt,
						CurrentDirectory:  "/work/mesh",
						DirectorySource:   protocol.DirectorySourceProcess,
						ForegroundCommand: "go test ./...",
						LastOutputAt:      &lastOutputAt,
						Attached:          true,
						Preview:           []string{"$ go test ./...", "ok"},
					},
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
				Type:        controlType,
				RequestID:   "control-1",
				SessionID:   "7K3D",
				Signal:      "term",
				Tail:        4096,
				PreviewCols: 80,
				PreviewRows: 6,
			})
			if err != nil || !handled {
				t.Fatalf("control handled = %v, error = %v", handled, err)
			}
			wantType := protocol.TypeOK
			if controlType == protocol.TypeLogs {
				wantType = protocol.TypeLogged
			} else if controlType == protocol.TypeInspect {
				wantType = protocol.TypeInspected
			}
			if response.Type != wantType || response.RequestID != "control-1" || response.SessionID != "7K3D" {
				t.Fatalf("control response = %+v", response)
			}
			if controlType == protocol.TypeLogs && !bytes.Equal(response.Output, []byte("recent output")) {
				t.Fatalf("logs output = %q", response.Output)
			}
			if controlType == protocol.TypeInspect && (response.Inspection == nil || response.Inspection.CurrentDirectory != "/work/mesh" || !response.Inspection.Attached) {
				t.Fatalf("inspection = %+v", response.Inspection)
			}
			if !workerConn.closed || len(workerConn.frames) != 1 {
				t.Fatalf("worker connection closed = %v, frames = %d", workerConn.closed, len(workerConn.frames))
			}
			if (controlType == protocol.TypeKill || controlType == protocol.TypeLogs || controlType == protocol.TypeInspect) && !workerConn.read {
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
			wantCols, wantRows := 0, 0
			if controlType == protocol.TypeInspect {
				wantCols, wantRows = 80, 6
			}
			if forwarded.Type != controlType || forwarded.SessionID != "7K3D" || forwarded.Signal != wantSignal || forwarded.Tail != wantTail || forwarded.PreviewCols != wantCols || forwarded.PreviewRows != wantRows {
				t.Fatalf("forwarded control = %+v", forwarded)
			}
		})
	}
}

func TestValidateInspectionResponseRejectsWorkerBoundaryViolations(t *testing.T) {
	t.Parallel()
	observedAt := time.Date(2026, time.September, 4, 9, 0, 0, 0, time.UTC)
	valid := protocol.SessionInspection{ObservedAt: observedAt, Preview: []string{"ready"}}
	response := func(sessionID string, inspection *protocol.SessionInspection) protocol.Frame {
		return controlFrame(t, protocol.Control{
			Type:       protocol.TypeInspected,
			RequestID:  "inspect-1",
			SessionID:  sessionID,
			Inspection: inspection,
		})
	}
	tooManyRows := valid
	tooManyRows.Preview = []string{"one", "two"}
	tooWide := valid
	tooWide.Preview = []string{"123456"}
	invalidDirectory := valid
	invalidDirectory.CurrentDirectory = "relative/path"
	invalidDirectory.DirectorySource = protocol.DirectorySourceProcess

	tests := []struct {
		name  string
		frame protocol.Frame
	}{
		{name: "non-control frame", frame: protocol.Frame{Kind: protocol.KindInput}},
		{name: "wrong session", frame: response("91AZ", &valid)},
		{name: "missing inspection", frame: response("7K3D", nil)},
		{name: "more rows than requested", frame: response("7K3D", &tooManyRows)},
		{name: "wider than requested", frame: response("7K3D", &tooWide)},
		{name: "invalid inspection", frame: response("7K3D", &invalidDirectory)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateInspectionResponse("7K3D", "inspect-1", 5, 1, test.frame); err == nil {
				t.Fatal("validateInspectionResponse() error = nil, want boundary rejection")
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

func TestLifecycleCreationIdentityIncludesTermAndDepth(t *testing.T) {
	launchCalls := 0
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog:     &lifecycleTestCatalog{},
		Connector:   failingLifecycleConnector(),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key"},
		SessionsDir: "/state/s",
		Launch: func(cfg worker.LaunchConfig) (worker.Launched, error) {
			launchCalls++
			if cfg.Term != "xterm-256color" || cfg.Depth != 1 {
				t.Fatalf("launch TERM = %q, depth = %d", cfg.Term, cfg.Depth)
			}
			return worker.Launched{Meta: worker.Meta{ID: "7K3D"}}, nil
		},
	})
	request := protocol.Control{
		Type: protocol.TypeCreate, RequestID: "launch-identity", Command: []string{"sh"}, Term: "xterm-256color", Depth: 1,
	}
	for range 2 {
		response, handled, err := lifecycle.HandleControl(context.Background(), request)
		if err != nil || !handled || response.SessionID != "7K3D" {
			t.Fatalf("identical create response = %+v, handled = %v, error = %v", response, handled, err)
		}
	}
	for _, field := range []string{"TERM", "depth"} {
		t.Run(field, func(t *testing.T) {
			changed := request
			if field == "TERM" {
				changed.Term = "vt100"
			} else {
				changed.Depth++
			}
			if _, handled, err := lifecycle.HandleControl(context.Background(), changed); !handled || err == nil {
				t.Errorf("conflicting %s retry handled = %v, error = %v; want rejection", field, handled, err)
			}
			if launchCalls != 1 {
				t.Fatalf("worker launches = %d, want 1", launchCalls)
			}
		})
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
		{Type: protocol.TypeInspect, RequestID: "request-5", SessionID: "7K3D", PreviewCols: protocol.MaxInspectionPreviewCols + 1, PreviewRows: 1},
		{Type: protocol.TypeHibernate, RequestID: "request-6", SessionID: "7K3D", HibernateIdleMillis: -1},
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

func TestLifecycleRejectsMalformedHibernateBeforeConnecting(t *testing.T) {
	for _, idle := range []int64{-1, -1 << 63, (1<<63-1)/int64(time.Millisecond) + 1, 288230376151711744, 1<<63 - 1} {
		t.Run(strconv.FormatInt(idle, 10), func(t *testing.T) {
			connectCalls := 0
			conn := &lifecycleRecordingConn{}
			lifecycle := mustLifecycle(t, lifecycleConfig{
				Catalog: &lifecycleTestCatalog{},
				Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
					connectCalls++
					return conn, nil
				}),
				Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key"},
				SessionsDir: "/state/s",
			})
			_, handled, err := lifecycle.HandleControl(context.Background(), protocol.Control{
				Type: protocol.TypeHibernate, RequestID: "invalid-idle", SessionID: "7K3D", HibernateIdleMillis: idle,
			})
			if !handled || err == nil || !strings.Contains(err.Error(), "session 7K3D") || !strings.Contains(err.Error(), "idle time") {
				t.Errorf("hibernate handled = %v, error = %v; want handled idle rejection naming the session", handled, err)
			}
			if connectCalls != 0 || conn.read || len(conn.frames) != 0 {
				t.Fatalf("invalid hibernate connected %d times, read = %v, writes = %d, closed = %v", connectCalls, conn.read, len(conn.frames), conn.closed)
			}
		})
	}
}

func TestLifecycleForwardsOneShotHibernateAcknowledgement(t *testing.T) {
	for _, test := range []struct {
		name     string
		response protocol.Control
		wantErr  string
		idle     int64
	}{
		{name: "accepted", response: protocol.Control{Type: protocol.TypeOK, RequestID: "hibernate-1", SessionID: "7K3D"}},
		{name: "largest idle", response: protocol.Control{Type: protocol.TypeOK, RequestID: "hibernate-1", SessionID: "7K3D"}, idle: (1<<63 - 1) / int64(time.Millisecond)},
		{name: "refused", response: protocol.Control{Type: protocol.TypeError, SessionID: "7K3D", Message: "not detached long enough"}, wantErr: "not detached long enough"},
		{name: "invalid acknowledgement", response: protocol.Control{Type: protocol.TypeOK, RequestID: "wrong-request", SessionID: "7K3D"}, wantErr: "invalid hibernation acknowledgement"},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := &lifecycleRecordingConn{readFrame: controlFrame(t, test.response)}
			lifecycle := mustLifecycle(t, lifecycleConfig{
				Catalog: &lifecycleTestCatalog{},
				Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
					return conn, nil
				}),
				Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key"},
				SessionsDir: "/state/s",
			})
			idle := test.idle
			if idle == 0 {
				idle = 250
			}
			response, handled, err := lifecycle.HandleControl(context.Background(), protocol.Control{
				Type: protocol.TypeHibernate, RequestID: "hibernate-1", SessionID: "7K3D", HibernateIdleMillis: idle,
			})
			if !handled || (test.wantErr == "" && err != nil) || (test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr))) {
				t.Fatalf("hibernate handled = %v, response = %+v, error = %v", handled, response, err)
			}
			if test.name == "refused" && err.Error() != test.wantErr {
				t.Fatalf("worker refusal changed: %v", err)
			}
			if !conn.closed || !conn.read || len(conn.frames) != 1 {
				t.Fatalf("closed = %v, read = %v, frames = %d", conn.closed, conn.read, len(conn.frames))
			}
			forwarded, err := protocol.DecodeControl(conn.frames[0].Payload)
			if err != nil || forwarded.HibernateIdleMillis != idle {
				t.Fatalf("forwarded hibernate = %+v, error = %v", forwarded, err)
			}
		})
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

func (c *lifecycleTestCatalog) Remove(_ context.Context, _ storage.SessionID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.retired++
	return nil
}

func TestRemoveRefusesARunningSession(t *testing.T) {
	t.Parallel()

	// Ending someone's work is a separate decision from tidying up after it,
	// and mesh kill already makes it.
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog: &lifecycleTestCatalog{sessions: []storage.Session{{ID: "7K3D", State: storage.StateRunning}}},
		Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
			return nil, errors.New("unexpected connect")
		}),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: t.TempDir(),
	})
	_, handled, err := lifecycle.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeRemove, RequestID: "rm-1", SessionID: "7K3D",
	})
	if !handled || err == nil || !strings.Contains(err.Error(), "kill it before removing it") {
		t.Fatalf("remove of a running session = %v", err)
	}
}

func TestRemoveDelegatesToCatalog(t *testing.T) {
	t.Parallel()

	sessionsDir := t.TempDir()
	catalog := &lifecycleTestCatalog{sessions: []storage.Session{{ID: "7K3D", State: storage.StateExited}}}
	lifecycle := mustLifecycle(t, lifecycleConfig{
		Catalog: catalog,
		Connector: lifecycleConnectorFunc(func(context.Context, protocol.SessionID) (transport.Conn, error) {
			return nil, errors.New("unexpected connect")
		}),
		Host:        storage.Host{ID: "host-a", MeshIdentity: "mesh-key", LastSeenAt: time.Now()},
		SessionsDir: sessionsDir,
	})

	response, handled, err := lifecycle.HandleControl(context.Background(), protocol.Control{
		Type: protocol.TypeRemove, RequestID: "rm-2", SessionID: "7K3D",
	})
	if err != nil || !handled || response.Type != protocol.TypeOK {
		t.Fatalf("remove = %+v, handled = %v, error = %v", response, handled, err)
	}
	if catalog.retired != 1 {
		t.Fatalf("catalog retired %d sessions, want 1", catalog.retired)
	}
}

func TestLifecycleCompletedCreationKeepsOnlyReplayData(t *testing.T) {
	const commandText = "private-command-text-must-not-stay-in-the-receipt"
	l := mustLifecycle(t, lifecycleConfig{
		Catalog: &lifecycleTestCatalog{}, Connector: failingLifecycleConnector(),
		Host: storage.Host{ID: "host-a", MeshIdentity: "mesh-key"}, SessionsDir: "/state/s",
		Launch: func(cfg worker.LaunchConfig) (worker.Launched, error) {
			return worker.Launched{Meta: worker.Meta{ID: "7K3D", Command: cfg.Command}}, nil
		},
	})
	if _, err := l.createSession(context.Background(), protocol.TypeCreate, "compact", creationRequest{command: []string{"sh", "-c", commandText}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%+v", *l.creations["compact"]), commandText) {
		t.Fatal("completed receipt still retains the full command or launch metadata")
	}
}

func receiptTestLifecycle(t *testing.T, now func() time.Time, launch launchWorker) *lifecycle {
	t.Helper()
	return mustLifecycle(t, lifecycleConfig{
		Catalog: &lifecycleTestCatalog{sessions: []storage.Session{{ID: "7K3D", State: storage.StateExited}}}, Connector: failingLifecycleConnector(),
		Host: storage.Host{ID: "host-a", MeshIdentity: "mesh-key"}, SessionsDir: "/state/s",
		Now: now, CreationRetention: time.Minute, Launch: launch,
	})
}

func TestLifecycleCreationReceiptsBoundedOverTime(t *testing.T) {
	now := time.Now()
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			l := receiptTestLifecycle(t, func() time.Time { return now }, func(worker.LaunchConfig) (worker.Launched, error) {
				if failed {
					return worker.Launched{}, errors.New("launch failed")
				}
				return worker.Launched{Meta: worker.Meta{ID: "7K3D"}}, nil
			})
			for i := range 1000 {
				_, err := l.createSession(context.Background(), protocol.TypeCreate, fmt.Sprintf("create-%d", i), creationRequest{command: []string{"sh"}})
				if (err != nil) != failed {
					t.Fatalf("create %d error = %v", i, err)
				}
				now = now.Add(10 * time.Second)
				if len(l.creations) > 13 {
					t.Fatalf("retained creations = %d, want at most 13", len(l.creations))
				}
			}
		})
	}
}

func TestLifecycleCreationReplayWindow(t *testing.T) {
	now := time.Now()
	launches := 0
	l := receiptTestLifecycle(t, func() time.Time { return now }, func(worker.LaunchConfig) (worker.Launched, error) {
		launches++
		return worker.Launched{}, errors.New("failed before starting a worker")
	})
	wanted := creationRequest{command: []string{"sh"}}
	for range 2 {
		_, _ = l.createSession(context.Background(), protocol.TypeCreate, "retry", wanted)
		now = now.Add(20 * time.Second)
	}
	if launches != 1 {
		t.Fatalf("launches inside retention = %d, want 1", launches)
	}
	now = now.Add(time.Minute)
	_, _ = l.createSession(context.Background(), protocol.TypeCreate, "retry", wanted)
	if launches != 2 {
		t.Fatalf("launches after expiry = %d, want 2", launches)
	}
}

func TestLifecycleLiveReceiptStartsRetentionAtObservedRetirement(t *testing.T) {
	now := time.Now()
	launches := 0
	l := receiptTestLifecycle(t, func() time.Time { return now }, func(worker.LaunchConfig) (worker.Launched, error) {
		launches++
		return worker.Launched{Meta: worker.Meta{ID: "7K3D"}}, nil
	})
	catalog := l.catalog.(*lifecycleTestCatalog)
	catalog.sessions = []storage.Session{{ID: "7K3D", State: storage.StateDetached}}
	wanted := creationRequest{command: []string{"sh"}}
	create := func() {
		t.Helper()
		if _, err := l.createSession(context.Background(), protocol.TypeCreate, "live", wanted); err != nil {
			t.Fatal(err)
		}
	}
	create()
	now = now.Add(24 * time.Hour)
	create()
	if launches != 1 {
		t.Fatalf("live session relaunched after retention, launches = %d", launches)
	}
	catalog.sessions = nil
	now = now.Add(time.Minute)
	create()
	if launches != 1 {
		t.Fatalf("session relaunched at observed retirement, launches = %d", launches)
	}
	now = now.Add(time.Minute - time.Nanosecond)
	create()
	if launches != 1 {
		t.Fatalf("session relaunched before retirement retention elapsed, launches = %d", launches)
	}
	now = now.Add(time.Nanosecond)
	create()
	if launches != 2 {
		t.Fatalf("launches after retirement retention = %d, want 2", launches)
	}
}

func TestLifecycleCreationPendingAndByteLimits(t *testing.T) {
	for _, budget := range []string{"pending", "bytes"} {
		t.Run(budget, func(t *testing.T) {
			now := time.Now()
			l := receiptTestLifecycle(t, func() time.Time { return now }, func(worker.LaunchConfig) (worker.Launched, error) {
				return worker.Launched{}, errors.New("unused")
			})
			wanted := creationRequest{command: []string{"sh"}}
			first, owner, err := l.creation("pending", wanted)
			if err != nil || !owner {
				t.Fatalf("first admission owner = %v, error = %v", owner, err)
			}
			if budget == "pending" {
				l.maxPendingCreations = 1
			} else {
				l.maxCreationBytes = l.creationBytes
			}
			now = now.Add(24 * time.Hour)
			if _, _, err := l.creation("new", wanted); err == nil || !strings.Contains(err.Error(), budget) {
				t.Fatalf("new admission error = %v, want contextual %s limit", err, budget)
			}
			replay, owner, err := l.creation("pending", wanted)
			if err != nil || owner || replay != first {
				t.Fatalf("pending replay owner = %v, error = %v", owner, err)
			}
			changed := wanted
			changed.command = []string{"other"}
			if _, _, err := l.creation("pending", changed); err == nil {
				t.Fatal("conflicting retry admitted at capacity")
			}
			l.forgetCreation("pending")
			if len(l.creations) != 1 {
				t.Fatal("forgot a pending creation")
			}
		})
	}
}

func TestLifecycleCreationBytesReleasedOnExpiryAndForget(t *testing.T) {
	now := time.Now()
	l := receiptTestLifecycle(t, func() time.Time { return now }, func(worker.LaunchConfig) (worker.Launched, error) {
		return worker.Launched{}, errors.New("launch failed")
	})
	wanted := creationRequest{command: []string{"sh", strings.Repeat("x", 10000)}}
	_, _ = l.createSession(context.Background(), protocol.TypeCreate, "expiry", wanted)
	if l.creationBytes == 0 || l.creationBytes >= 10000 {
		t.Fatalf("completed retained bytes = %d, want compact nonzero receipt", l.creationBytes)
	}
	now = now.Add(time.Minute)
	_, _ = l.createSession(context.Background(), protocol.TypeCreate, "forget", wanted)
	if len(l.creations) != 1 {
		t.Fatalf("receipts after expiry = %d, want 1", len(l.creations))
	}
	l.forgetCreation("forget")
	if l.creationBytes != 0 || l.pendingCreations != 0 || l.completedCreations.Len() != 0 {
		t.Fatalf("accounting after forget = %d bytes, %d pending, %d queued", l.creationBytes, l.pendingCreations, l.completedCreations.Len())
	}
}

func TestLifecycleConcurrentCreationAdmissionsRespectPendingCap(t *testing.T) {
	now := time.Now()
	l := receiptTestLifecycle(t, func() time.Time { return now }, func(worker.LaunchConfig) (worker.Launched, error) { return worker.Launched{}, nil })
	l.maxPendingCreations = 4
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			if _, owner, err := l.creation(fmt.Sprintf("concurrent-%d", i), creationRequest{command: []string{"sh"}}); err == nil && owner {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 4 || l.pendingCreations != 4 {
		t.Fatalf("admitted = %d, pending = %d, want 4", admitted.Load(), l.pendingCreations)
	}
}

func TestLifecycleReceiptLookupOnlyWhenDueAndPreservesInconclusiveRetirement(t *testing.T) {
	now := time.Now()
	launches := 0
	l := receiptTestLifecycle(t, func() time.Time { return now }, func(worker.LaunchConfig) (worker.Launched, error) {
		launches++
		return worker.Launched{Meta: worker.Meta{ID: "7K3D"}}, nil
	})
	wanted := creationRequest{command: []string{"sh"}}
	for range 100 {
		if _, err := l.createSession(context.Background(), protocol.TypeCreate, "lookup", wanted); err != nil {
			t.Fatal(err)
		}
	}
	catalog := l.catalog.(*lifecycleTestCatalog)
	if catalog.getCalls != 1 {
		t.Fatalf("catalog lookups before expiry = %d, want only the publication lookup", catalog.getCalls)
	}
	catalog.getErr = errors.New("catalog unavailable")
	for range 3 {
		now = now.Add(time.Hour)
		if _, err := l.createSession(context.Background(), protocol.TypeCreate, "lookup", wanted); err != nil {
			t.Fatal(err)
		}
	}
	if launches != 1 || catalog.getCalls != 4 {
		t.Fatalf("launches = %d, lookups = %d, want 1 and 4", launches, catalog.getCalls)
	}
}

func TestLifecycleCreationFingerprintIncludesEveryLaunchField(t *testing.T) {
	wanted := creationRequest{command: []string{"sh", "-c", "ab"}, cwd: "/work", cols: 80, rows: 24, term: "xterm", depth: 1, label: "route", env: []string{"A=b"}}
	l := receiptTestLifecycle(t, time.Now, func(worker.LaunchConfig) (worker.Launched, error) { return worker.Launched{}, errors.New("unused") })
	if _, _, err := l.creation("identity", wanted); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []creationRequest{
		{command: []string{"sh", "-c", "a", "b"}, cwd: wanted.cwd, cols: wanted.cols, rows: wanted.rows, term: wanted.term, depth: wanted.depth, label: wanted.label, env: wanted.env},
		{command: wanted.command, cwd: "/elsewhere", cols: wanted.cols, rows: wanted.rows, term: wanted.term, depth: wanted.depth, label: wanted.label, env: wanted.env},
		{command: wanted.command, cwd: wanted.cwd, cols: 81, rows: wanted.rows, term: wanted.term, depth: wanted.depth, label: wanted.label, env: wanted.env},
		{command: wanted.command, cwd: wanted.cwd, cols: wanted.cols, rows: 25, term: wanted.term, depth: wanted.depth, label: wanted.label, env: wanted.env},
		{command: wanted.command, cwd: wanted.cwd, cols: wanted.cols, rows: wanted.rows, term: "vt100", depth: wanted.depth, label: wanted.label, env: wanted.env},
		{command: wanted.command, cwd: wanted.cwd, cols: wanted.cols, rows: wanted.rows, term: wanted.term, depth: 2, label: wanted.label, env: wanted.env},
		{command: wanted.command, cwd: wanted.cwd, cols: wanted.cols, rows: wanted.rows, term: wanted.term, depth: wanted.depth, label: "other", env: wanted.env},
		{command: wanted.command, cwd: wanted.cwd, cols: wanted.cols, rows: wanted.rows, term: wanted.term, depth: wanted.depth, label: wanted.label, env: []string{"A=c"}},
	} {
		if _, _, err := l.creation("identity", changed); err == nil {
			t.Fatalf("admitted conflicting launch fields %+v", changed)
		}
	}
}

func TestLifecycleCreationCompletedFailureBoundsErrorBytes(t *testing.T) {
	l := receiptTestLifecycle(t, time.Now, func(worker.LaunchConfig) (worker.Launched, error) {
		return worker.Launched{}, errors.New(strings.Repeat("failure", 10000))
	})
	l.maxCreationBytes = int64(creationReceiptBytes + len("large-error") + session.IDLen + maxCreationErrorBytes)
	_, _ = l.createSession(context.Background(), protocol.TypeCreate, "large-error", creationRequest{})
	if len(l.creations) != 1 || l.creationBytes > l.maxCreationBytes {
		t.Fatalf("error receipt count = %d, bytes = %d, budget = %d", len(l.creations), l.creationBytes, l.maxCreationBytes)
	}
	if len(l.creations["large-error"].launchErr.Error()) != maxCreationErrorBytes {
		t.Fatal("error replay did not cap retained error text")
	}
}

func TestLifecycleCreationStartedFailureFitsReservedBytes(t *testing.T) {
	var l *lifecycle
	var reserved int64
	l = receiptTestLifecycle(t, time.Now, func(worker.LaunchConfig) (worker.Launched, error) {
		reserved = l.creationBytes
		return worker.Launched{}, &worker.StartedError{ID: "7K3D", Err: errors.New(strings.Repeat("e", maxCreationErrorBytes))}
	})
	id, err := l.createSession(context.Background(), protocol.TypeCreate, "started-error", creationRequest{})
	if err == nil || id != "7K3D" {
		t.Fatalf("started worker identity = %q, error = %v", id, err)
	}
	if l.creationBytes > reserved {
		t.Fatalf("completed bytes = %d exceed admission reservation %d", l.creationBytes, reserved)
	}
}

func TestLifecycleCreationByteLimitRefusesLaunchButReplaysCompleted(t *testing.T) {
	launches := 0
	l := receiptTestLifecycle(t, time.Now, func(worker.LaunchConfig) (worker.Launched, error) {
		launches++
		return worker.Launched{Meta: worker.Meta{ID: "7K3D"}}, nil
	})
	wanted := creationRequest{command: []string{"sh"}}
	if _, err := l.createSession(context.Background(), protocol.TypeCreate, "complete", wanted); err != nil {
		t.Fatal(err)
	}
	l.maxCreationBytes = l.creationBytes
	if _, err := l.createSession(context.Background(), protocol.TypeCreate, "new", wanted); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("new creation at byte capacity error = %v", err)
	}
	if id, err := l.createSession(context.Background(), protocol.TypeCreate, "complete", wanted); err != nil || id != "7K3D" {
		t.Fatalf("completed replay at capacity = %q, %v", id, err)
	}
	if launches != 1 {
		t.Fatalf("launches at capacity = %d, want 1", launches)
	}
}

func TestLifecycleCreationRetirementClockIncludesCatalogLookupTime(t *testing.T) {
	now := time.Now()
	launches := 0
	l := receiptTestLifecycle(t, func() time.Time { return now }, func(worker.LaunchConfig) (worker.Launched, error) {
		launches++
		return worker.Launched{Meta: worker.Meta{ID: "7K3D"}}, nil
	})
	wanted := creationRequest{command: []string{"sh"}}
	create := func() {
		t.Helper()
		if _, err := l.createSession(context.Background(), protocol.TypeCreate, "lookup-clock", wanted); err != nil {
			t.Fatal(err)
		}
	}
	create()
	catalog := l.catalog.(*lifecycleTestCatalog)
	catalog.getHook = func() { now = now.Add(30 * time.Second) }
	now = now.Add(time.Minute)
	create()
	catalog.getHook = nil
	now = now.Add(time.Minute - time.Nanosecond)
	create()
	if launches != 1 {
		t.Fatalf("session relaunched before a full window after catalog lookup, launches = %d", launches)
	}
	now = now.Add(time.Nanosecond)
	create()
	if launches != 2 {
		t.Fatalf("launches after observed retirement window = %d, want 2", launches)
	}
}

type receiptPublicationFailureStore struct{ *storage.Store }

func (s *receiptPublicationFailureStore) ApplyHostChanges(context.Context, storage.HostID, storage.HostChanges) error {
	return errors.New("publication rejected")
}

func unpublishedReceiptLifecycle(t *testing.T, failure string, now func() time.Time, launches *int) *lifecycle {
	t.Helper()
	root := t.TempDir()
	store, err := storage.Open(t.Context(), filepath.Join(root, "receipt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	host := catalogTestHost(now())
	catalog, err := NewCatalog(CatalogConfig{
		SessionsDir: root, Host: host, Store: &receiptPublicationFailureStore{Store: store},
		Probe: probeFunc(func(context.Context, string) error { return nil }), BootID: func() string { return "boot-a" }, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return mustLifecycle(t, lifecycleConfig{
		Catalog: catalog, Connector: failingLifecycleConnector(), Host: host, SessionsDir: root,
		Now: now, CreationRetention: time.Minute,
		Launch: func(worker.LaunchConfig) (worker.Launched, error) {
			*launches++
			id := []string{"7K3D", "8M4F", "9P5G"}[*launches-1]
			dir := filepath.Join(root, id)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			fakeOwnedWorker(t, dir, id)
			if failure == "started" {
				if err := os.Remove(paths.Meta(dir)); err != nil {
					t.Fatal(err)
				}
				return worker.Launched{}, &worker.StartedError{ID: id, Err: errors.New("metadata unavailable after start")}
			}
			meta := catalogTestMeta(id, worker.StateRunning, "boot-a")
			meta.CreatedAt = now()
			meta.PID = os.Getpid()
			if err := worker.WriteMeta(dir, meta); err != nil {
				t.Fatal(err)
			}
			return worker.Launched{Meta: meta}, nil
		},
	})
}

func TestLifecycleUnpublishedReceiptLives(t *testing.T) {
	for _, failure := range []string{"publication", "started"} {
		t.Run(failure, func(t *testing.T) {
			now := time.Now()
			launches := 0
			l := unpublishedReceiptLifecycle(t, failure, func() time.Time { return now }, &launches)
			wanted := creationRequest{command: []string{"sh"}}
			for range 3 {
				id, err := l.createSession(t.Context(), protocol.TypeCreate, "unpublished", wanted)
				if err == nil || id != "7K3D" {
					t.Fatalf("live unpublished replay = %q, %v", id, err)
				}
				now = now.Add(time.Minute)
			}
			if launches != 1 {
				t.Fatalf("unpublished live worker launches = %d, want 1", launches)
			}
		})
	}
}

func TestLifecycleUnpublishedReceiptExpiresAfterExit(t *testing.T) {
	for _, failure := range []string{"publication", "started"} {
		t.Run(failure, func(t *testing.T) {
			now := time.Now()
			launches := 0
			l := unpublishedReceiptLifecycle(t, failure, func() time.Time { return now }, &launches)
			wanted := creationRequest{command: []string{"sh"}}
			id, err := l.createSession(t.Context(), protocol.TypeCreate, "unpublished-exit", wanted)
			if err == nil || id != "7K3D" {
				t.Fatalf("unpublished creation = %q, %v", id, err)
			}
			meta := catalogTestMeta(id, worker.StateExited, "boot-a")
			meta.CreatedAt = now
			meta.ExitedAt = &now
			code := 0
			meta.ExitCode = &code
			if err := worker.WriteMeta(filepath.Join(l.sessionsDir, id), meta); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			_, _ = l.createSession(t.Context(), protocol.TypeCreate, "unpublished-exit", wanted)
			if launches != 1 {
				t.Fatalf("relaunch at observed worker exit = %d, want 1", launches)
			}
			now = now.Add(time.Minute)
			_, _ = l.createSession(t.Context(), protocol.TypeCreate, "unpublished-exit", wanted)
			if launches != 2 {
				t.Fatalf("launches after confirmed exit retention = %d, want 2", launches)
			}
		})
	}
}

func TestLifecycleReceiptRemembersPublishedCatalogRow(t *testing.T) {
	now := time.Now()
	launches := 0
	l := receiptTestLifecycle(t, func() time.Time { return now }, func(worker.LaunchConfig) (worker.Launched, error) {
		launches++
		return worker.Launched{Meta: worker.Meta{ID: "7K3D"}}, nil
	})
	catalog := l.catalog.(*lifecycleTestCatalog)
	catalog.sessions = []storage.Session{{ID: "7K3D", State: storage.StateDetached}}
	wanted := creationRequest{command: []string{"sh"}}
	if _, err := l.createSession(t.Context(), protocol.TypeCreate, "rapid-retirement", wanted); err != nil {
		t.Fatal(err)
	}
	catalog.sessions = nil
	for range 2 {
		now = now.Add(time.Minute)
		if _, err := l.createSession(t.Context(), protocol.TypeCreate, "rapid-retirement", wanted); err != nil {
			t.Fatal(err)
		}
	}
	if launches != 2 {
		t.Fatalf("launches after confirmed published-row retirement = %d, want 2", launches)
	}
}

func TestLifecycleCreationDetachesMapRequestID(t *testing.T) {
	l := receiptTestLifecycle(t, time.Now, func(worker.LaunchConfig) (worker.Launched, error) {
		return worker.Launched{}, nil
	})
	requestID := strings.Repeat("x", 1<<20)[:4]
	created, _, err := l.creation(requestID, creationRequest{command: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if reflect.ValueOf(created.requestID).Pointer() == reflect.ValueOf(requestID).Pointer() {
		t.Fatal("receipt request ID retains the caller's backing allocation")
	}
	for key := range l.creations {
		if reflect.ValueOf(key).Pointer() == reflect.ValueOf(requestID).Pointer() {
			t.Fatal("map request ID retains the caller's backing allocation")
		}
	}
}
