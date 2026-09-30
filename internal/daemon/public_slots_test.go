package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
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

func TestPublicSlotsEvictOldestIdleConnection(t *testing.T) {
	base, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := newBoundedPublicListener(base, 2)
	idle := make(chan struct{}, 3)
	server := &http.Server{ReadHeaderTimeout: httpReadHeaderTimeout, ConnState: func(c net.Conn, state http.ConnState) {
		listener.connState(c, state)
		if state == http.StateIdle {
			idle <- struct{}{}
		}
	},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.closeActive() })
	first, err := publicSlotRequest(base.Addr().String(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	waitSignal(t, idle, "first idle connection")
	second, err := publicSlotRequest(base.Addr().String(), "127.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	waitSignal(t, idle, "second idle connection")
	third, err := publicSlotRequest(base.Addr().String(), "127.0.0.3")
	if err != nil {
		t.Fatalf("new source was not admitted to full idle pool: %v", err)
	}
	defer func() { _ = third.Close() }()
	_ = first.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := first.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("oldest idle connection read = %v, want EOF", err)
	}
	_ = second.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(second, "GET / HTTP/1.1\r\nHost: app.example.test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(second), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("newer idle connection was evicted: %v", err)
	}
	_ = response.Body.Close()
}

func TestPublicSourceIPv6QuotaAggregatesPrefix(t *testing.T) {
	base := &queuedPublicListener{connections: make(chan net.Conn, 34), accepted: make(chan struct{}, 35), closed: make(chan struct{})}
	for i := range 34 {
		server, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		address := fmt.Sprintf("2001:db8:1::%x", i+1)
		if i == 33 {
			address = "2001:db8:2::1"
		}
		base.connections <- publicAddressedConn{Conn: server, address: &net.TCPAddr{IP: net.ParseIP(address), Port: 1234}}
	}
	listener := newBoundedPublicListener(base, 512)
	t.Cleanup(func() { _ = listener.Close(); _ = listener.closeActive() })
	for range 32 {
		if _, err := listener.Accept(); err != nil {
			t.Fatal(err)
		}
	}
	last, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if got := last.RemoteAddr().String(); got != "[2001:db8:2::1]:1234" {
		t.Fatalf("over-share IPv6 address admitted: %s", got)
	}
}

func TestPublicBodyProgressAllowsLongUpload(t *testing.T) {
	server := httptest.NewUnstartedServer(guardPublicBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), 100*time.Millisecond))
	server.Config.ConnContext = publicConnectionContext
	server.Start()
	defer server.Close()
	connection, err := net.Dial("tcp4", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(connection, "POST / HTTP/1.1\r\nHost: app.example.test\r\nContent-Length: 40960\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := io.WriteString(connection, strings.Repeat("x", 8192)); err != nil {
			t.Fatal(err)
		}
		<-time.After(50 * time.Millisecond)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("healthy upload status %d", response.StatusCode)
	}
}

func TestPublicBodyMinimumRateRejectsTrickle(t *testing.T) {
	server := httptest.NewUnstartedServer(guardPublicBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			w.Header().Set("Connection", "close")
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), 100*time.Millisecond))
	server.Config.ConnContext = publicConnectionContext
	server.Start()
	defer server.Close()
	connection, err := net.Dial("tcp4", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(connection, "POST / HTTP/1.1\r\nHost: app.example.test\r\nContent-Length: 40960\r\n\r\nx"); err != nil {
		t.Fatal(err)
	}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for range 20 {
			<-time.After(20 * time.Millisecond)
			if _, err := io.WriteString(connection, "x"); err != nil {
				return
			}
		}
	}()
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("trickle status %d", response.StatusCode)
	}
	_ = connection.Close()
	<-writerDone
}

func TestPublicBodyResponseStartedExemptsDuplexStream(t *testing.T) {
	server := httptest.NewUnstartedServer(guardPublicBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		controller := http.NewResponseController(w)
		if err := controller.EnableFullDuplex(); err != nil {
			t.Error(err)
			return
		}
		_, _ = io.WriteString(w, "first\n")
		if err := controller.Flush(); err != nil {
			t.Error(err)
			return
		}
		_, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = io.WriteString(w, "second\n")
	}), 100*time.Millisecond))
	server.Config.ConnContext = publicConnectionContext
	server.Start()
	defer server.Close()
	connection, err := net.Dial("tcp4", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(connection, "POST / HTTP/1.1\r\nHost: app.example.test\r\nContent-Length: 2\r\nConnection: close\r\n\r\nx"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	reader := bufio.NewReader(response.Body)
	if first, err := reader.ReadString('\n'); err != nil || first != "first\n" {
		t.Fatalf("first chunk = %q, %v", first, err)
	}
	<-time.After(150 * time.Millisecond)
	if _, err := io.WriteString(connection, "y"); err != nil {
		t.Fatal(err)
	}
	if second, err := reader.ReadString('\n'); err != nil || second != "second\n" {
		t.Fatalf("second chunk = %q, %v", second, err)
	}
}

func TestPublicBodyTimeoutPreservesOtherHTTP2Stream(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	server := httptest.NewUnstartedServer(guardPublicBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/upload" {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "second\n")
	}), 100*time.Millisecond))
	server.EnableHTTP2 = true
	server.Config.ConnContext = publicConnectionContext
	server.StartTLS()
	defer server.Close()
	client := server.Client()
	client.Timeout = time.Second
	client.Transport.(*http.Transport).MaxConnsPerHost = 1
	stream, err := client.Get(server.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()
	if stream.ProtoMajor != 2 {
		t.Fatalf("protocol = %s, want HTTP/2", stream.Proto)
	}
	reader := bufio.NewReader(stream.Body)
	if first, err := reader.ReadString('\n'); err != nil || first != "first\n" {
		t.Fatalf("first chunk = %q, %v", first, err)
	}
	body, writer := io.Pipe()
	defer func() { _ = body.Close(); _ = writer.Close() }()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		response, err := client.Post(server.URL+"/upload", "application/octet-stream", body)
		if err == nil {
			_ = response.Body.Close()
		}
	}()
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("HTTP/2 body deadline did not expire")
	}
	unblock()
	if second, err := reader.ReadString('\n'); err != nil || second != "second\n" {
		t.Fatalf("other HTTP/2 stream was cut off: %q, %v", second, err)
	}
}

func TestPublicBodyEarlyResponseDoesNotParkUnconsumedBody(t *testing.T) {
	server := httptest.NewUnstartedServer(guardPublicBody(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.(http.Flusher).Flush()
	}), 100*time.Millisecond))
	server.Config.ConnContext = publicConnectionContext
	server.Start()
	defer server.Close()
	connection, err := net.Dial("tcp4", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(connection, "POST / HTTP/1.1\r\nHost: app.example.test\r\nContent-Length: 10\r\n\r\nx"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("early response blocked draining a stalled body: %v", err)
	}
	_ = response.Body.Close()
	if _, err := connection.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("unconsumed body retained socket: %v", err)
	}
}

func TestPublicBodyUpgradeClearsReadDeadline(t *testing.T) {
	server := httptest.NewUnstartedServer(guardPublicBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
			return
		}
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = connection.CloseNow() }()
		_, _, _ = connection.Read(r.Context())
	}), 100*time.Millisecond))
	server.Config.ConnContext = publicConnectionContext
	server.Start()
	defer server.Close()
	connection, err := net.Dial("tcp4", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(connection, "GET /socket HTTP/1.1\r\nHost: app.example.test\r\nContent-Length: 1\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\nx"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status %d", response.StatusCode)
	}
	<-time.After(150 * time.Millisecond)
	if _, err := connection.Write([]byte{0x89, 0x81, 1, 2, 3, 4, 'x' ^ 1}); err != nil {
		t.Fatal(err)
	}
	pong := make([]byte, 3)
	if _, err := io.ReadFull(reader, pong); err != nil {
		t.Fatalf("upgraded socket stopped after the body deadline: %v", err)
	}
	if !bytes.Equal(pong, []byte{0x8a, 1, 'x'}) {
		t.Fatalf("pong = %x", pong)
	}
}

func TestPublicListenerPreservesTemporaryAcceptErrors(t *testing.T) {
	listener := newBoundedPublicListener(failingListener{err: &net.OpError{Op: "accept", Err: syscall.EINTR}}, 2)
	_, err := listener.Accept()
	transient, ok := err.(net.Error)   //nolint:errorlint // net/http uses a direct assertion before retrying Accept
	if !ok || !transient.Temporary() { //nolint:staticcheck // net/http still uses Temporary to retry Accept
		t.Fatalf("Accept error lost net/http retry semantics: %v", err)
	}
}

func TestPublicProxyExpiryPreventsLateAdmission(t *testing.T) {
	closeStarted := make(chan struct{})
	allowClose := make(chan struct{})
	var allowOnce sync.Once
	unblock := func() { allowOnce.Do(func() { close(allowClose) }) }
	defer unblock()
	server, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	base := &queuedPublicListener{connections: make(chan net.Conn, 1), accepted: make(chan struct{}, 1), closed: make(chan struct{})}
	base.connections <- publicBlockingCloseConn{Conn: server, started: closeStarted, release: allowClose}
	listener := newBoundedPublicListener(base, 512)
	listener.proxyUIDs = []uint32{0}
	t.Cleanup(func() { unblock(); _ = listener.Close(); _ = listener.closeActive() })
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	waitSignal(t, closeStarted, "expired PROXY socket close")
	admitted := listener.admit(connection.(*boundedPublicConn), netip.MustParsePrefix("198.51.100.1/32"))
	unblock()
	if admitted {
		t.Fatal("an expired authentication socket was promoted while Close was finishing")
	}
}

type publicBlockingCloseConn struct {
	net.Conn
	started chan struct{}
	release <-chan struct{}
}

func (c publicBlockingCloseConn) Close() error {
	close(c.started)
	<-c.release
	if err := c.Conn.Close(); err != nil {
		return fmt.Errorf("test socket close: %w", err)
	}
	return nil
}

func TestPublicLateProxyExpiryPreservesAdmittedConnection(t *testing.T) {
	server, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	base := &queuedPublicListener{connections: make(chan net.Conn, 1), accepted: make(chan struct{}, 1), closed: make(chan struct{})}
	base.connections <- publicAddressedConn{Conn: server, address: &net.TCPAddr{IP: net.IPv4(198, 51, 100, 1), Port: 1234}}
	listener := newBoundedPublicListener(base, 512)
	t.Cleanup(func() { _ = listener.Close(); _ = listener.closeActive() })
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	listener.expireAuthentication(connection.(*boundedPublicConn))
	listener.mu.Lock()
	admitted := connection.(*boundedPublicConn).admitted
	listener.mu.Unlock()
	if !admitted {
		t.Fatal("a late authentication deadline closed an admitted source")
	}
}
