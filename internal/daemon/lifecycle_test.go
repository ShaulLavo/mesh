package daemon

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

type lifecycleTestCatalog struct {
	sessions       []storage.Session
	reconcileErr   error
	reconcileCalls int
}

func (c *lifecycleTestCatalog) Reconcile(context.Context) error {
	c.reconcileCalls++
	return c.reconcileErr
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
	if catalog.reconcileCalls != 1 {
		t.Fatalf("reconciliations = %d, want 1", catalog.reconcileCalls)
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
	for _, controlType := range []string{protocol.TypeSignal, protocol.TypeKill} {
		t.Run(controlType, func(t *testing.T) {
			workerConn := &lifecycleRecordingConn{}
			lifecycle := mustLifecycle(t, lifecycleConfig{
				Catalog: &lifecycleTestCatalog{},
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
			})
			if err != nil || !handled {
				t.Fatalf("control handled = %v, error = %v", handled, err)
			}
			if response.Type != protocol.TypeOK || response.RequestID != "control-1" || response.SessionID != "7K3D" {
				t.Fatalf("control response = %+v", response)
			}
			if !workerConn.closed || len(workerConn.frames) != 1 {
				t.Fatalf("worker connection closed = %v, frames = %d", workerConn.closed, len(workerConn.frames))
			}
			forwarded, err := protocol.DecodeControl(workerConn.frames[0].Payload)
			if err != nil {
				t.Fatal(err)
			}
			wantSignal := ""
			if controlType == protocol.TypeSignal {
				wantSignal = "term"
			}
			if forwarded.Type != controlType || forwarded.SessionID != "7K3D" || forwarded.Signal != wantSignal {
				t.Fatalf("forwarded control = %+v", forwarded)
			}
		})
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
		{Type: protocol.TypeCreate, RequestID: "request-1"},
		{Type: protocol.TypeSignal, RequestID: "request-2", SessionID: "7K3D", Signal: "bogus"},
		{Type: protocol.TypeKill, RequestID: "request-3", SessionID: "../X"},
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
	if _, handled, err := lifecycle.HandleControl(nil, protocol.Control{Type: protocol.TypeList, RequestID: "request-4"}); !handled || err == nil {
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
	frames []protocol.Frame
	closed bool
}

func (*lifecycleRecordingConn) ReadFrame() (protocol.Frame, error) {
	return protocol.Frame{}, errors.New("unexpected read")
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
