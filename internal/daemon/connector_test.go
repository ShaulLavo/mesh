package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

type lookupFunc func(context.Context, storage.SessionID) (storage.Session, error)

func (f lookupFunc) Get(ctx context.Context, id storage.SessionID) (storage.Session, error) {
	return f(ctx, id)
}

func TestWorkerConnectorResolvesCatalogSessionAndCarriesFrames(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "7K3D")
	if err := os.Mkdir(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", paths.Socket(sessionDir))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	connector, err := newWorkerConnector(root, lookupFunc(func(_ context.Context, id storage.SessionID) (storage.Session, error) {
		if id != "7K3D" {
			t.Fatalf("catalog ID = %q, want 7K3D", id)
		}
		return storage.Session{ID: id, State: storage.StateRunning}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	sid, err := protocol.NewSessionID("7K3D")
	if err != nil {
		t.Fatal(err)
	}

	serverFrames := make(chan protocol.Frame, 1)
	serverErr := make(chan error, 1)
	go func() {
		stream, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		conn, err := transport.NewStreamConn(stream)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		frame, err := conn.ReadFrame()
		if err == nil {
			serverFrames <- frame
		}
		serverErr <- err
	}()

	conn, err := connector.ConnectWorker(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	want := protocol.Frame{Kind: protocol.KindInput, Session: sid, Payload: []byte("input")}
	if err := conn.WriteFrame(want); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	got := <-serverFrames
	if got.Kind != want.Kind || got.Session != want.Session || string(got.Payload) != string(want.Payload) {
		t.Fatalf("worker frame = %+v, want %+v", got, want)
	}
}

func TestWorkerConnectorRejectsUnroutableSessionsBeforeDial(t *testing.T) {
	lookupCalls := 0
	connector, err := newWorkerConnector(t.TempDir(), lookupFunc(func(_ context.Context, id storage.SessionID) (storage.Session, error) {
		lookupCalls++
		return storage.Session{ID: id, State: storage.StateExited}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	for _, text := range []string{"../X", "7k3d", "TOO-LONG"} {
		sid, err := protocol.NewSessionID(text)
		if err != nil {
			if text == "TOO-LONG" {
				continue
			}
			t.Fatal(err)
		}
		if _, err := connector.ConnectWorker(context.Background(), sid); err == nil {
			t.Fatalf("ConnectWorker accepted noncanonical ID %q", text)
		}
	}
	if lookupCalls != 0 {
		t.Fatalf("catalog lookups for invalid IDs = %d, want 0", lookupCalls)
	}

	sid, _ := protocol.NewSessionID("7K3D")
	if _, err := connector.ConnectWorker(context.Background(), sid); err == nil {
		t.Fatal("ConnectWorker accepted an exited session")
	}
	if lookupCalls != 1 {
		t.Fatalf("catalog lookups = %d, want 1", lookupCalls)
	}
}

func TestWorkerConnectorPreservesLookupAndContextErrors(t *testing.T) {
	wantLookup := errors.New("lookup failed")
	connector, err := newWorkerConnector(t.TempDir(), lookupFunc(func(context.Context, storage.SessionID) (storage.Session, error) {
		return storage.Session{}, wantLookup
	}))
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := protocol.NewSessionID("7K3D")
	if _, err := connector.ConnectWorker(context.Background(), sid); !errors.Is(err, wantLookup) {
		t.Fatalf("lookup error = %v, want errors.Is lookup failure", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	connector, err = newWorkerConnector(t.TempDir(), lookupFunc(func(context.Context, storage.SessionID) (storage.Session, error) {
		return storage.Session{ID: "7K3D", State: storage.StateRunning}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := connector.ConnectWorker(ctx, sid); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled dial error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled dial took %v", elapsed)
	}
}
