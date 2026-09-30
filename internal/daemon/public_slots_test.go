package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/transport"
)

func TestPublicSlotsOneSourceCannotStarveAnother(t *testing.T) {
	base, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := newBoundedPublicListener(base, maximumPublicConnections)
	server := &http.Server{ReadHeaderTimeout: httpReadHeaderTimeout, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.closeActive() })
	served := 0
	for range maximumPublicConnections {
		connection, err := publicSlotRequest(base.Addr().String(), "127.0.0.1")
		if connection != nil {
			t.Cleanup(func() { _ = connection.Close() })
		}
		if err == nil {
			served++
		}
	}
	connection, err := publicSlotRequest(base.Addr().String(), "127.0.0.2")
	if connection != nil {
		defer func() { _ = connection.Close() }()
	}
	if err != nil {
		t.Fatalf("second source blocked after first source parked %d connections: %v", served, err)
	}
	if served > 32 {
		t.Fatalf("one source parked %d connections, want at most 32", served)
	}
	t.Logf("first source parked %d connections; second source served", served)
}

func publicSlotRequest(address, source string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(source)}}
	connection, err := dialer.Dial("tcp4", address)
	if err != nil {
		return nil, fmt.Errorf("dial public slot: %w", err)
	}
	_ = connection.SetDeadline(time.Now().Add(250 * time.Millisecond))
	if _, err := io.WriteString(connection, "GET / HTTP/1.1\r\nHost: app.example.test\r\n\r\n"); err != nil {
		return connection, fmt.Errorf("request public slot: %w", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodGet})
	if err != nil {
		return connection, fmt.Errorf("request public slot: %w", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return connection, fmt.Errorf("status %d", response.StatusCode)
	}
	return connection, nil
}

func TestPublicBodyTrickleReleasesSlot(t *testing.T) {
	unixListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publicListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		_ = unixListener.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveBoundListeners(ctx, cancel, listenerConfig{
			webSocketPath: "/mesh", reporter: newErrorReporter(nil), shutdownTimeout: time.Second,
			publicReadTimeout: 100 * time.Millisecond,
			publicHTTPHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := io.Copy(io.Discard, r.Body)
				if err != nil {
					w.Header().Set("Connection", "close")
					w.WriteHeader(http.StatusRequestTimeout)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}),
		}, func(context.Context, transport.Conn) error { return nil }, unixListener, nil, nil, publicListener)
	}()
	t.Cleanup(func() {
		cancel()
		if err := waitRuntime(t, done); err != nil {
			t.Error(err)
		}
	})
	connection, err := net.Dial("tcp4", publicListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(connection, "POST / HTTP/1.1\r\nHost: app.example.test\r\nContent-Length: 1048576\r\n\r\nx"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	_, err = io.Copy(io.Discard, reader)
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatalf("slow-body connection kept its slot after timeout response %q", status)
	}
}
