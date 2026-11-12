package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/transport"
)

func TestServeReportsClientCloseFailuresWithoutFailingShutdown(t *testing.T) {
	for _, row := range []struct {
		name string
		err  error
	}{
		{"peer EOF", io.EOF},
		{"truncated peer frame", io.ErrUnexpectedEOF},
		{"invalid peer frame", errors.New("invalid WebSocket opcode")},
	} {
		t.Run(row.name, func(t *testing.T) { testServeClientClose(t, row.err, nil, nil) })
	}
}

func TestServeClientClosePreservesListenerFailures(t *testing.T) {
	t.Run("accept", func(t *testing.T) {
		testServeClientClose(t, io.EOF, errors.New("accept failed"), nil)
	})
	t.Run("close", func(t *testing.T) {
		testServeClientClose(t, io.EOF, nil, errors.New("listener close failed"))
	})
}

func testServeClientClose(t *testing.T, clientError, acceptError, listenerError error) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	listener := &clientCloseListener{
		acceptThenFailListener: &acceptThenFailListener{first: make(chan net.Conn, 1), fail: make(chan struct{}, 1), err: net.ErrClosed},
		closeError:             listenerError,
	}
	if acceptError != nil {
		listener.err = acceptError
	}
	closed := make(chan struct{})
	listener.first <- &clientCloseConn{Conn: server, err: clientError, closed: closed}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	reports := make(chan error, 1)
	reporter := newErrorReporter(func(err error) { reports <- err })
	t.Cleanup(reporter.shutdown)
	started, stopped := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- serveBoundListeners(ctx, cancel, listenerConfig{reporter: reporter}, func(ctx context.Context, _ transport.Conn) error {
			close(started)
			<-ctx.Done()
			<-closed
			close(stopped)
			return nil
		}, listener, nil, nil, nil)
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(runtimeTestTimeout):
			t.Error("shutdown fixture did not stop")
		}
	})
	waitSignal(t, started, "client handler startup")
	if acceptError != nil {
		listener.fail <- struct{}{}
	} else {
		cancel()
	}
	err := waitRuntime(t, done)
	if acceptError == nil && listenerError == nil && err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if acceptError != nil && !errors.Is(err, acceptError) {
		t.Fatalf("shutdown = %v, want accept failure %v", err, acceptError)
	}
	if listenerError != nil && !errors.Is(err, listenerError) {
		t.Fatalf("shutdown = %v, want listener close failure %v", err, listenerError)
	}
	if errors.Is(err, clientError) {
		t.Fatalf("client error became daemon failure: %v", err)
	}
	waitSignal(t, stopped, "client handler completion")
	var report error
	select {
	case report = <-reports:
	case report = <-reporter.queue:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("client close failure was not submitted to the reporter")
	}
	if !errors.Is(report, clientError) {
		t.Fatalf("reported = %v, want client error %v", report, clientError)
	}
}

type clientCloseConn struct {
	net.Conn
	err    error
	closed chan struct{}
}

func (c *clientCloseConn) Close() error {
	err := c.Conn.Close()
	close(c.closed)
	return errors.Join(err, c.err)
}

type clientCloseListener struct {
	*acceptThenFailListener
	closeError error
}

func (l *clientCloseListener) Close() error {
	return errors.Join(l.acceptThenFailListener.Close(), l.closeError)
}
