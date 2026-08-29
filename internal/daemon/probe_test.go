package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestUnixWorkerProbeConnectsAndCloses(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "worker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()

	if err := newUnixWorkerProbe().Probe(context.Background(), listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-acceptErr:
		t.Fatal(err)
	case conn := <-accepted:
		defer conn.Close()
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		var one [1]byte
		if _, err := conn.Read(one[:]); !errors.Is(err, io.EOF) {
			t.Fatalf("read after probe = %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not accept probe")
	}
}

func TestUnixWorkerProbeValidatesBoundaryAndCancellation(t *testing.T) {
	probe := newUnixWorkerProbe()
	if err := probe.Probe(nil, "/tmp/worker.sock"); err == nil {
		t.Fatal("nil context accepted")
	}
	if err := probe.Probe(context.Background(), ""); err == nil {
		t.Fatal("empty socket path accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := probe.Probe(ctx, filepath.Join(t.TempDir(), "missing.sock")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe error = %v, want context.Canceled", err)
	}
}
