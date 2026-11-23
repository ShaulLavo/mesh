package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestClassifyDialNamesResolvedAddressAndCause(t *testing.T) {
	resolved := &net.TCPAddr{IP: net.ParseIP("fd7a:115c:a1e0::5e36:5e5f"), Port: 7337}
	dial := &net.OpError{Op: "dial", Net: "tcp", Addr: resolved, Err: os.NewSyscallError("connect", syscall.EPERM)}
	err := fmt.Errorf("failed to WebSocket dial: %w", dial)
	connect := ClassifyDial("pc.example.ts.net:7337", err)
	if connect == nil {
		t.Fatal("TCP dial failure was not classified")
	}
	want := "TCP connection to [fd7a:115c:a1e0::5e36:5e5f]:7337 was blocked by local permissions (operation not permitted)"
	if connect.Problem() != want || connect.Failure != ConnectBlocked || !connect.TCP {
		t.Fatalf("connect = %+v, problem %q", connect, connect.Problem())
	}
	if !errors.Is(connect, syscall.EPERM) {
		t.Fatalf("%q lost EPERM", connect)
	}
}

func TestClassifyWebSocketDialDeadlineWithoutDialError(t *testing.T) {
	err := fmt.Errorf("failed to send handshake request: %w", fmt.Errorf("Get: %w", context.DeadlineExceeded))
	connect := classifyWebSocketDial("ws://pc.example.ts.net:7337/mesh", err)
	if connect == nil {
		t.Fatal("deadline before the upgrade was not classified")
	}
	want := "control connection to pc.example.ts.net:7337 timed out before Mesh authentication (context deadline exceeded)"
	if connect.Problem() != want || connect.TCP || !errors.Is(connect, context.DeadlineExceeded) {
		t.Fatalf("connect = %+v, problem %q", connect, connect.Problem())
	}
}

func TestClassifyWebSocketDialLeavesCancellationAlone(t *testing.T) {
	canceled := fmt.Errorf("failed to WebSocket dial: %w", &net.OpError{Op: "dial", Net: "tcp", Err: context.Canceled})
	for _, err := range []error{context.Canceled, canceled, errors.New("HTTP 503")} {
		if connect := classifyWebSocketDial("ws://pc:7337/mesh", err); connect != nil {
			t.Fatalf("%v classified as %q", err, connect)
		}
	}
}

func TestDialOnceToSilentListenerReportsControlTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// The goroutine sends exactly once, nil when Accept fails after Close, so
	// cleanup can wait for it and close whatever it accepted.
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			conn = nil
		}
		accepted <- conn
	}()
	defer func() {
		_ = listener.Close()
		if conn := <-accepted; conn != nil {
			_ = conn.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = DialOnce(ctx, "ws://"+listener.Addr().String()+"/mesh", DialOptions{})
	var connect *ConnectError
	if !errors.As(err, &connect) || connect.Failure != ConnectTimedOut {
		t.Fatalf("error = %v, want a control connection timeout", err)
	}
	if connect.Address != listener.Addr().String() {
		t.Fatalf("address = %q, want %q", connect.Address, listener.Addr())
	}
}

func TestDialOnceToClosedPortReportsRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = DialOnce(context.Background(), "ws://"+address+"/mesh", DialOptions{})
	want := "transport: nothing accepted the TCP connection to " + address + " (connection refused)"
	if err == nil || err.Error() != want || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("error = %v, want %q", err, want)
	}
}
