//go:build mesh_installed_fixture

package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/transporttrace"
)

func TestDaemonLifecycleRecordsShutdownOrderingWithoutNativeServices(t *testing.T) {
	directory := t.TempDir()
	recorder := transporttrace.New(directory, transporttrace.Identity{PID: os.Getpid(), Version: "v0.1.9001",
		Commit: strings.Repeat("a", 40), Digest: strings.Repeat("b", 64)})
	t.Cleanup(recorder.Close)
	ctx, cancel := context.WithCancel(transporttrace.WithRecorder(t.Context(), recorder))
	t.Cleanup(cancel)
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	listener := &clientCloseListener{acceptThenFailListener: &acceptThenFailListener{
		first: make(chan net.Conn, 1), fail: make(chan struct{}, 1), err: net.ErrClosed}}
	listener.first <- server
	started := make(chan struct{})
	reporter := newErrorReporter(nil)
	t.Cleanup(reporter.shutdown)
	done := make(chan error, 1)
	go func() {
		done <- serveBoundListeners(ctx, cancel, listenerConfig{reporter: reporter}, func(ctx context.Context, _ transport.Conn) error {
			close(started)
			<-ctx.Done()
			return nil
		}, listener, nil, nil, nil)
	}()
	waitSignal(t, started, "in-memory handler")
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, recorder.Done(), "lifecycle writer completion")
	files, _ := filepath.Glob(filepath.Join(directory, "*.jsonl"))
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	previous := -1
	for _, stage := range []string{"daemon.cancel", "daemon.canceled", "groups.close_begin", "groups.close_end", "http.shutdown_begin", "http.shutdown_end", "daemon.end", "process.end"} {
		index := strings.Index(string(data), `"stage":"`+stage+`"`)
		if index <= previous {
			t.Fatalf("shutdown stage %s is absent/out of order: %s", stage, data)
		}
		previous = index
	}
}
