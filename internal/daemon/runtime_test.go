package daemon

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/shaul/mesh/internal/dnsname"
	"github.com/shaul/mesh/internal/edge"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

const runtimeTestTimeout = 3 * time.Second

func TestErrorReporterWorkerStopsAfterHealthySink(t *testing.T) {
	reported := make(chan error, 1)
	reporter := newErrorReporter(func(err error) { reported <- err })
	want := errors.New("report")
	reporter.report(want)
	select {
	case got := <-reported:
		if !errors.Is(got, want) {
			t.Fatalf("reported = %v, want %v", got, want)
		}
	case <-time.After(runtimeTestTimeout):
		t.Fatal("healthy report sink was not called")
	}
	reporter.shutdown()
	select {
	case <-reporter.stop:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("healthy reporter worker did not stop")
	}
}

func TestServeCarriesUnixFramesAndCancellationUnblocksHandler(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	handlerBlocked := make(chan struct{})
	done := runRuntime(t, ctx, ListenerConfig{StateDir: stateDir}, func(_ context.Context, conn transport.Conn) error {
		frame, err := conn.ReadFrame()
		if err != nil {
			return err
		}
		if err := conn.WriteFrame(frame); err != nil {
			return err
		}
		close(handlerBlocked)
		_, err = conn.ReadFrame()
		return err
	})

	client := dialUnixRuntime(t, filepath.Join(stateDir, daemonSocketName))
	sid, err := protocol.NewSessionID("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	want := protocol.Frame{Kind: protocol.KindData, Session: sid, Seq: 42, Payload: []byte("hello")}
	if err := client.WriteFrame(want); err != nil {
		t.Fatal(err)
	}
	got, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	assertRuntimeFrame(t, got, want)
	waitSignal(t, handlerBlocked, "handler to block on its connection")

	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatalf("Serve after cancellation: %v", err)
	}
	if _, err := client.ReadFrame(); err == nil {
		t.Fatal("client connection remained open after runtime cancellation")
	}
	if _, err := os.Lstat(filepath.Join(stateDir, daemonSocketName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon socket after shutdown: %v", err)
	}
}

func TestServeWaitsForConnectionHandlersDuringShutdown(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	handlerStarted := make(chan struct{})
	handlerCancelled := make(chan struct{})
	releaseHandler := make(chan struct{})
	done := runRuntime(t, ctx, ListenerConfig{StateDir: stateDir}, func(ctx context.Context, _ transport.Conn) error {
		close(handlerStarted)
		<-ctx.Done()
		close(handlerCancelled)
		<-releaseHandler
		return nil
	})
	conn := dialUnixRuntime(t, filepath.Join(stateDir, daemonSocketName))
	defer conn.Close() //nolint:errcheck // test cleanup
	waitSignal(t, handlerStarted, "handler startup")

	cancel()
	waitSignal(t, handlerCancelled, "handler cancellation")
	select {
	case err := <-done:
		t.Fatalf("Serve returned before its handler: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseHandler)
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestServeReplacesProvenStaleUnixSocket(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	socketPath := filepath.Join(stateDir, daemonSocketName)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("stale socket was not preserved by the fixture: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	accepted := make(chan struct{}, 1)
	done := runRuntime(t, ctx, ListenerConfig{StateDir: stateDir}, func(context.Context, transport.Conn) error {
		accepted <- struct{}{}
		return nil
	})
	conn := dialUnixRuntime(t, socketPath)
	waitSignal(t, accepted, "replacement listener to accept a connection")
	_ = conn.Close()
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestServeEnforcesSingleOwnerWithoutUnlinkingLiveSocket(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	firstAccepted := make(chan struct{}, 1)
	firstDone := runRuntime(t, ctx, ListenerConfig{StateDir: stateDir}, func(ctx context.Context, _ transport.Conn) error {
		firstAccepted <- struct{}{}
		<-ctx.Done()
		return nil
	})
	socketPath := filepath.Join(stateDir, daemonSocketName)
	waitForPath(t, socketPath)

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- Serve(context.Background(), ListenerConfig{StateDir: stateDir}, func(context.Context, transport.Conn) error {
			return nil
		})
	}()
	secondErr := waitRuntime(t, secondDone)
	if !errors.Is(secondErr, ErrDaemonAlreadyRunning) {
		t.Fatalf("second Serve error = %v, want ErrDaemonAlreadyRunning", secondErr)
	}

	conn := dialUnixRuntime(t, socketPath)
	waitSignal(t, firstAccepted, "first daemon to remain reachable")
	_ = conn.Close()
	cancel()
	if err := waitRuntime(t, firstDone); err != nil {
		t.Fatal(err)
	}
}

func TestServeDoesNotRemoveAReplacementAtTheDaemonSocketPath(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := runRuntime(t, ctx, ListenerConfig{StateDir: stateDir}, func(context.Context, transport.Conn) error {
		return nil
	})
	socketPath := filepath.Join(stateDir, daemonSocketName)
	waitForPath(t, socketPath)
	if err := os.Remove(socketPath); err != nil {
		t.Fatal(err)
	}
	want := []byte("replacement\n")
	if err := os.WriteFile(socketPath, want, 0o600); err != nil {
		t.Fatal(err)
	}

	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(socketPath) //nolint:gosec // test reads its own temporary replacement fixture
	if err != nil {
		t.Fatalf("replacement path was removed: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("replacement contents = %q, want %q", got, want)
	}
}

func TestServeDoesNotRemoveAReplacementUnixSocket(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := runRuntime(t, ctx, ListenerConfig{StateDir: stateDir}, func(context.Context, transport.Conn) error {
		return nil
	})
	socketPath := filepath.Join(stateDir, daemonSocketName)
	waitForPath(t, socketPath)
	if err := os.Remove(socketPath); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close() //nolint:errcheck // test cleanup

	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
	probe, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatalf("replacement Unix socket was removed: %v", err)
	}
	_ = probe.Close()
}

func TestServeDoesNotReplaceAReachableForeignUnixSocket(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	socketPath := filepath.Join(stateDir, daemonSocketName)
	foreign, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close() //nolint:errcheck // test cleanup

	err = Serve(context.Background(), ListenerConfig{StateDir: stateDir}, func(context.Context, transport.Conn) error {
		return nil
	})
	if !errors.Is(err, ErrDaemonAlreadyRunning) {
		t.Fatalf("Serve error = %v, want ErrDaemonAlreadyRunning", err)
	}
	probe, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatalf("foreign socket was replaced: %v", err)
	}
	_ = probe.Close()
}

func TestServeAllowsLocalOnlyConfiguration(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := runRuntime(t, ctx, ListenerConfig{StateDir: stateDir}, func(context.Context, transport.Conn) error {
		return nil
	})
	conn := dialUnixRuntime(t, filepath.Join(stateDir, daemonSocketName))
	_ = conn.Close()
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestServeWebSocketUsesExactAddressAndPath(t *testing.T) {
	t.Parallel()

	port := reserveTCPPort(t, "127.0.0.1")
	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := runRuntime(t, ctx, ListenerConfig{
		StateDir:      stateDir,
		TailnetAddrs:  []string{"127.0.0.1"},
		TailnetPort:   port,
		WebSocketPath: "/mesh",
	}, echoOneFrame)

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHTTPStatus(t, baseURL+"/wrong", http.StatusNotFound)
	if conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.2:%d", port), 100*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("WebSocket listener accepted a connection on an address that was not supplied")
	}

	dialCtx, dialCancel := context.WithTimeout(context.Background(), runtimeTestTimeout)
	defer dialCancel()
	client, err := transport.Dial(dialCtx, fmt.Sprintf("ws://127.0.0.1:%d/mesh", port), transport.DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sid, err := protocol.NewSessionID("ABCD")
	if err != nil {
		t.Fatal(err)
	}
	want := protocol.Frame{Kind: protocol.KindInput, Session: sid, Payload: []byte("x")}
	if err := client.WriteFrame(want); err != nil {
		t.Fatal(err)
	}
	got, err := client.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	assertRuntimeFrame(t, got, want)
	_ = client.Close()

	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestServeHTTPSUsesLoopbackServicesOnlyAndHotReloads(t *testing.T) {
	port := reserveTCPPort(t, "127.0.0.1")
	store, err := dnsname.NewBundleStore(filepath.Join(t.TempDir(), "tls"), dnsname.WildcardName)
	if err != nil {
		t.Fatal(err)
	}
	source, err := dnsname.NewCertificateSource(store)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	services := http.NewServeMux()
	service := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("X-Mesh-Route", "service")
		_, _ = io.WriteString(w, request.URL.EscapedPath()) //nolint:gosec // test response deliberately echoes a URL path
	})
	services.Handle("/mesh", service)
	services.Handle("/service", service)
	done := runRuntime(t, ctx, ListenerConfig{
		StateDir: stateDir, HTTPSPort: port, WebSocketPath: "/mesh",
		TLSConfig:   &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: source.GetCertificate},
		HTTPHandler: services,
	}, func(context.Context, transport.Conn) error {
		return errors.New("terminal handler reached from HTTPS")
	})
	waitForTCPRuntime(t, net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	if connection, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.2", fmt.Sprint(port)), 100*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("HTTPS listener accepted a non-configured loopback address")
	}
	dialer := &net.Dialer{Timeout: time.Second}
	if connection, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), &tls.Config{ //nolint:gosec // the missing-certificate handshake is the assertion
		InsecureSkipVerify: true, //nolint:gosec // the local self-signed certificate is not yet installed
	}); err == nil {
		_ = connection.Close()
		t.Fatal("TLS handshake succeeded without an installed certificate")
	}

	now := time.Now().UTC()
	firstCertificate, firstKey := daemonTestCertificate(t, 1, now)
	if _, err := source.Install(firstCertificate, firstKey); err != nil {
		t.Fatal(err)
	}
	assertHTTPSCertificateAndRoute(t, port, firstCertificate, 1)

	secondCertificate, secondKey := daemonTestCertificate(t, 2, now)
	if _, err := source.Install(secondCertificate, secondKey); err != nil {
		t.Fatal(err)
	}
	assertHTTPSCertificateAndRoute(t, port, secondCertificate, 2)

	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestServeFailsWhenHTTPSPortCannotBind(t *testing.T) {
	blocked, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()                              //nolint:errcheck // test cleanup
	port := uint16(blocked.Addr().(*net.TCPAddr).Port) //nolint:gosec // net.TCPAddr ports are bounded to uint16
	stateDir := t.TempDir()
	err = Serve(context.Background(), ListenerConfig{
		StateDir: stateDir, HTTPSPort: port, WebSocketPath: "/mesh", HTTPHandler: http.NotFoundHandler(),
		TLSConfig: &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return nil, dnsname.ErrNoCertificate
		}},
	}, echoOneFrame)
	if err == nil || !strings.Contains(err.Error(), "bind HTTPS") {
		t.Fatalf("Serve error = %v, want HTTPS bind failure", err)
	}
	if _, statErr := os.Lstat(filepath.Join(stateDir, daemonSocketName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("daemon socket after failed HTTPS startup: %v", statErr)
	}
}

func TestServeReportsPartialTailnetBindFailure(t *testing.T) {
	t.Parallel()

	blocked, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()                              //nolint:errcheck // test cleanup
	port := uint16(blocked.Addr().(*net.TCPAddr).Port) //nolint:gosec // net.TCPAddr ports are bounded to uint16
	reports := make(chan error, 4)
	stateDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := runRuntime(t, ctx, ListenerConfig{
		StateDir:      stateDir,
		TailnetAddrs:  []string{"127.0.0.1", "127.0.0.2"},
		TailnetPort:   port,
		WebSocketPath: "/mesh",
		ReportError:   func(err error) { reports <- err },
	}, echoOneFrame)

	select {
	case report := <-reports:
		if !strings.Contains(report.Error(), "127.0.0.1") {
			t.Fatalf("partial bind report = %v, want failed address", report)
		}
	case <-time.After(runtimeTestTimeout):
		t.Fatal("partial tailnet bind failure was not reported")
	}
	waitForHTTPStatus(t, fmt.Sprintf("http://127.0.0.2:%d/wrong", port), http.StatusNotFound)

	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestServeFailsWhenNoTailnetAddressCanBind(t *testing.T) {
	t.Parallel()

	blocked, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()                              //nolint:errcheck // test cleanup
	port := uint16(blocked.Addr().(*net.TCPAddr).Port) //nolint:gosec // net.TCPAddr ports are bounded to uint16
	stateDir := t.TempDir()
	err = Serve(context.Background(), ListenerConfig{
		StateDir:      stateDir,
		TailnetAddrs:  []string{"127.0.0.1"},
		TailnetPort:   port,
		WebSocketPath: "/mesh",
	}, echoOneFrame)
	if err == nil || !strings.Contains(err.Error(), "bind tailnet") {
		t.Fatalf("Serve error = %v, want tailnet bind failure", err)
	}
	if _, statErr := os.Lstat(filepath.Join(stateDir, daemonSocketName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("daemon socket after failed startup: %v", statErr)
	}
}

func TestServeRejectsInvalidBoundaryConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  func(string) ListenerConfig
		want string
	}{
		{name: "empty state directory", cfg: func(string) ListenerConfig { return ListenerConfig{} }, want: "state directory"},
		{name: "missing state directory", cfg: func(dir string) ListenerConfig { return ListenerConfig{StateDir: filepath.Join(dir, "missing")} }, want: "state directory"},
		{name: "invalid address", cfg: func(dir string) ListenerConfig {
			return ListenerConfig{StateDir: dir, TailnetAddrs: []string{"not-an-ip"}, TailnetPort: 1, WebSocketPath: "/mesh"}
		}, want: "Tailnet address"},
		{name: "wildcard address", cfg: func(dir string) ListenerConfig {
			return ListenerConfig{StateDir: dir, TailnetAddrs: []string{"0.0.0.0"}, TailnetPort: 1, WebSocketPath: "/mesh"}
		}, want: "concrete"},
		{name: "zero port", cfg: func(dir string) ListenerConfig {
			return ListenerConfig{StateDir: dir, TailnetAddrs: []string{"127.0.0.1"}, WebSocketPath: "/mesh"}
		}, want: "port"},
		{name: "relative WebSocket path", cfg: func(dir string) ListenerConfig {
			return ListenerConfig{StateDir: dir, TailnetAddrs: []string{"127.0.0.1"}, TailnetPort: 1, WebSocketPath: "mesh"}
		}, want: "WebSocket path"},
		{name: "unclean WebSocket path", cfg: func(dir string) ListenerConfig {
			return ListenerConfig{StateDir: dir, TailnetAddrs: []string{"127.0.0.1"}, TailnetPort: 1, WebSocketPath: "/a/../mesh"}
		}, want: "WebSocket path"},
		{name: "escaped WebSocket path", cfg: func(dir string) ListenerConfig {
			return ListenerConfig{StateDir: dir, TailnetAddrs: []string{"127.0.0.1"}, TailnetPort: 1, WebSocketPath: "/m%65sh"}
		}, want: "WebSocket path"},
		{name: "backslash WebSocket path", cfg: func(dir string) ListenerConfig {
			return ListenerConfig{StateDir: dir, TailnetAddrs: []string{"127.0.0.1"}, TailnetPort: 1, WebSocketPath: `/mesh\child`}
		}, want: "WebSocket path"},
		{name: "force-query WebSocket path", cfg: func(dir string) ListenerConfig {
			return ListenerConfig{StateDir: dir, TailnetAddrs: []string{"127.0.0.1"}, TailnetPort: 1, WebSocketPath: "/mesh?"}
		}, want: "WebSocket path"},
		{name: "TLS config without HTTPS port", cfg: func(dir string) ListenerConfig {
			return ListenerConfig{StateDir: dir, TLSConfig: &tls.Config{}}
		}, want: "HTTPS port"},
		{name: "HTTPS without service handler", cfg: func(dir string) ListenerConfig {
			return ListenerConfig{StateDir: dir, HTTPSPort: 8443, WebSocketPath: "/mesh", TLSConfig: &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }}}
		}, want: "service handler"},
		{name: "HTTPS without certificate source", cfg: func(dir string) ListenerConfig {
			return ListenerConfig{StateDir: dir, HTTPSPort: 8443, WebSocketPath: "/mesh", HTTPHandler: http.NotFoundHandler(), TLSConfig: &tls.Config{}}
		}, want: "GetCertificate"},
		{name: "old TLS version", cfg: func(dir string) ListenerConfig {
			return ListenerConfig{StateDir: dir, HTTPSPort: 8443, WebSocketPath: "/mesh", HTTPHandler: http.NotFoundHandler(), TLSConfig: &tls.Config{ //nolint:gosec // invalid-version fixture exercises the TLS 1.2 boundary
				MinVersion:     tls.VersionTLS11,
				GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil },
			}}
		}, want: "TLS 1.2"},
		{name: "HTTPS without reserved path", cfg: func(dir string) ListenerConfig {
			return ListenerConfig{StateDir: dir, HTTPSPort: 8443, HTTPHandler: http.NotFoundHandler(), TLSConfig: &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }}}
		}, want: "WebSocket path"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateDir := t.TempDir()
			err := Serve(context.Background(), tt.cfg(stateDir), echoOneFrame)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Serve error = %v, want text %q", err, tt.want)
			}
			if _, statErr := os.Lstat(filepath.Join(stateDir, daemonSocketName)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("daemon socket created for invalid configuration: %v", statErr)
			}
		})
	}

	stateDir := t.TempDir()
	if err := Serve(context.Background(), ListenerConfig{StateDir: stateDir}, nil); err == nil || !strings.Contains(err.Error(), "handler") {
		t.Fatalf("nil handler error = %v", err)
	}
}

func TestServeDoesNotTouchWorkerArtifacts(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	workerDir := filepath.Join(stateDir, "s", "ABCD")
	if err := os.MkdirAll(workerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(workerDir, "meta.json")
	wantMeta := []byte("worker-owned\n")
	if err := os.WriteFile(metaPath, wantMeta, 0o600); err != nil {
		t.Fatal(err)
	}
	workerPath := filepath.Join(workerDir, "sock")
	workerListener, err := net.ListenUnix("unix", &net.UnixAddr{Name: workerPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer workerListener.Close() //nolint:errcheck // test cleanup

	ctx, cancel := context.WithCancel(context.Background())
	done := runRuntime(t, ctx, ListenerConfig{StateDir: stateDir}, func(context.Context, transport.Conn) error { return nil })
	waitForPath(t, filepath.Join(stateDir, daemonSocketName))
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}

	gotMeta, err := os.ReadFile(metaPath) //nolint:gosec // test reads its own temporary worker metadata fixture
	if err != nil {
		t.Fatal(err)
	}
	if string(gotMeta) != string(wantMeta) {
		t.Fatalf("worker metadata = %q, want %q", gotMeta, wantMeta)
	}
	probe, err := net.DialTimeout("unix", workerPath, time.Second)
	if err != nil {
		t.Fatalf("worker socket stopped with daemon: %v", err)
	}
	_ = probe.Close()
}

func TestUnixAcceptLoopReturnsUnexpectedFailure(t *testing.T) {
	t.Parallel()

	want := errors.New("accept exploded")
	err := serveUnixConnections(context.Background(), failingListener{err: want}, newConnectionGroup(func(context.Context, transport.Conn) error {
		return nil
	}))
	if !errors.Is(err, want) {
		t.Fatalf("accept error = %v, want %v", err, want)
	}
}

func TestServeBoundListenersCancelsSharedContextBeforeWaitingForHandlers(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })
	want := errors.New("accept exploded")
	listener := &acceptThenFailListener{
		first: make(chan net.Conn, 1),
		fail:  make(chan struct{}, 1),
		err:   want,
	}
	listener.first <- serverConn

	runCtx, cancel := context.WithCancel(context.Background())
	handlerStarted := make(chan struct{})
	handlerCancelled := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- serveBoundListeners(
			runCtx,
			cancel,
			listenerConfig{reporter: newErrorReporter(nil)},
			func(ctx context.Context, _ transport.Conn) error {
				close(handlerStarted)
				<-ctx.Done()
				close(handlerCancelled)
				return ctx.Err()
			},
			listener,
			nil,
			nil,
			nil,
		)
	}()

	waitSignal(t, handlerStarted, "handler startup")
	listener.fail <- struct{}{}
	waitSignal(t, handlerCancelled, "shared daemon cancellation")
	if err := waitRuntime(t, done); !errors.Is(err, want) {
		t.Fatalf("listener core error = %v, want %v", err, want)
	}
	if !errors.Is(runCtx.Err(), context.Canceled) {
		t.Fatalf("shared daemon context error = %v, want context.Canceled", runCtx.Err())
	}
}

func TestServeBoundListenersForcesStuckServiceRequestClosed(t *testing.T) {
	unixListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		_ = unixListener.Close()
		t.Fatal(err)
	}
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	requestDone := make(chan error, 1)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveBoundListeners(
			runCtx,
			cancel,
			listenerConfig{
				webSocketPath: "/mesh", reporter: newErrorReporter(nil), shutdownTimeout: 25 * time.Millisecond,
				httpHandler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					close(requestStarted)
					<-releaseRequest
				}),
			},
			func(context.Context, transport.Conn) error { return nil },
			unixListener,
			[]net.Listener{httpListener},
			nil,
			nil,
		)
	}()
	go func() {
		response, err := (&http.Client{Timeout: runtimeTestTimeout}).Get("http://" + httpListener.Addr().String() + "/stuck")
		if response != nil {
			_ = response.Body.Close()
		}
		requestDone <- err
	}()
	waitSignal(t, requestStarted, "stuck service request")
	cancel()
	serveErr := waitRuntime(t, done)
	if !errors.Is(serveErr, context.DeadlineExceeded) {
		t.Fatalf("forced shutdown error = %v, want deadline exceeded", serveErr)
	}
	close(releaseRequest)
	select {
	case <-requestDone:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("stuck client request did not unblock after forced close")
	}
}

func TestPublicServerGivesInFlightRequestShutdownGrace(t *testing.T) {
	unixListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publicListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		_ = unixListener.Close()
		t.Fatal(err)
	}
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	clientDone := make(chan error, 1)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveBoundListeners(
			runCtx, cancel,
			listenerConfig{
				webSocketPath: "/mesh", reporter: newErrorReporter(nil), shutdownTimeout: time.Second,
				publicHTTPHandler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
					close(requestStarted)
					<-releaseRequest
					response.WriteHeader(http.StatusNoContent)
				}),
			},
			func(context.Context, transport.Conn) error { return nil }, unixListener, nil, nil, publicListener,
		)
	}()
	go func() {
		response, err := (&http.Client{Timeout: runtimeTestTimeout}).Get("http://" + publicListener.Addr().String() + "/stream")
		if response != nil {
			if response.StatusCode != http.StatusNoContent && err == nil {
				err = fmt.Errorf("status %d", response.StatusCode)
			}
			_ = response.Body.Close()
		}
		clientDone <- err
	}()
	waitSignal(t, requestStarted, "public request startup")
	cancel()
	select {
	case err := <-done:
		t.Fatalf("public server returned before the request completed: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseRequest)
	if err := <-clientDone; err != nil {
		t.Fatal(err)
	}
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestPublicServerClosesHijackedWebSocketOnShutdown(t *testing.T) {
	unixListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publicListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		_ = unixListener.Close()
		t.Fatal(err)
	}
	hijacked := make(chan struct{})
	handlerDone := make(chan struct{})
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- serveBoundListeners(
			runCtx, cancel,
			listenerConfig{
				webSocketPath: "/mesh", reporter: newErrorReporter(nil), shutdownTimeout: time.Second,
				publicReadTimeout: 200 * time.Millisecond,
				publicHTTPHandler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
					connection, acceptErr := websocket.Accept(response, request, nil)
					if acceptErr != nil {
						close(handlerDone)
						return
					}
					close(hijacked)
					_, _, _ = connection.Read(context.Background())
					close(handlerDone)
				}),
			},
			func(context.Context, transport.Conn) error { return nil }, unixListener, nil, nil, publicListener,
		)
	}()
	ctx, stop := context.WithTimeout(context.Background(), runtimeTestTimeout)
	defer stop()
	connection, response, err := websocket.Dial(ctx, "ws://"+publicListener.Addr().String()+"/socket", nil)
	if response != nil && response.Body != nil {
		defer response.Body.Close() //nolint:errcheck // test cleanup
	}
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow() //nolint:errcheck // test connection cleanup
	clientReadDone := make(chan error, 1)
	go func() {
		_, _, readErr := connection.Read(ctx)
		clientReadDone <- readErr
	}()
	waitSignal(t, hijacked, "public WebSocket upgrade")
	select {
	case <-time.After(250 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := connection.Ping(ctx); err != nil {
		t.Fatalf("public WebSocket stopped after the HTTP read timeout: %v", err)
	}
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handlerDone:
	case <-time.After(runtimeTestTimeout):
		t.Fatal("hijacked public WebSocket handler did not unblock")
	}
	select {
	case readErr := <-clientReadDone:
		if readErr == nil {
			t.Fatal("public WebSocket remained open after daemon shutdown")
		}
	case <-time.After(runtimeTestTimeout):
		t.Fatal("public WebSocket client did not observe daemon shutdown")
	}
}

func TestPublicServerTimesOutIncompleteProxiedBody(t *testing.T) {
	unixListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publicListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		_ = unixListener.Close()
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		response.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	origin, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := edge.NewRegistry(edge.HandlerConfig{Mode: edge.ModeProxy, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.Replace([]edge.PublishedRoute{{
		Route: edge.Route{PublicName: "app.shaulavo.dev", ServiceName: "app"},
		Origin: edge.ResolvedOrigin{
			Identity: origin.ID, DisplayAlias: "Desktop", Endpoint: netip.MustParseAddrPort(strings.TrimPrefix(backend.URL, "http://")),
			LastSeenAt: now, OnlineUntil: now.Add(time.Hour), Online: true,
		},
	}}); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- serveBoundListeners(
			runCtx, cancel,
			listenerConfig{
				webSocketPath: "/mesh", reporter: newErrorReporter(nil), shutdownTimeout: time.Second,
				publicReadTimeout: 100 * time.Millisecond,
				publicHTTPHandler: registry,
			},
			func(context.Context, transport.Conn) error { return nil }, unixListener, nil, nil, publicListener,
		)
	}()
	connection, err := net.DialTimeout("tcp4", publicListener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close() //nolint:errcheck // test cleanup
	_ = connection.SetDeadline(time.Now().Add(runtimeTestTimeout))
	if _, err := io.WriteString(connection, "POST /app HTTP/1.1\r\nHost: app.shaulavo.dev\r\nContent-Length: 10\r\nX-Forwarded-For: 198.51.100.1\r\nX-Forwarded-Proto: https\r\n\r\na"); err != nil {
		t.Fatal(err)
	}
	status, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil || !strings.Contains(status, " 408 ") {
		t.Fatalf("incomplete-body response = %q, %v", status, err)
	}
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestPublicReadTimeoutDoesNotStopStreamingResponse(t *testing.T) {
	unixListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publicListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		_ = unixListener.Close()
		t.Fatal(err)
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseResponse := func() { releaseOnce.Do(func() { close(release) }) }
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		releaseResponse()
		cancel()
	})
	done := make(chan error, 1)
	go func() {
		done <- serveBoundListeners(
			runCtx, cancel,
			listenerConfig{
				webSocketPath: "/mesh", reporter: newErrorReporter(nil), shutdownTimeout: time.Second,
				publicReadTimeout: 100 * time.Millisecond,
				publicHTTPHandler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(response, "first\n")
					response.(http.Flusher).Flush()
					<-release
					_, _ = io.WriteString(response, "second\n")
				}),
			},
			func(context.Context, transport.Conn) error { return nil }, unixListener, nil, nil, publicListener,
		)
	}()
	response, err := (&http.Client{Timeout: runtimeTestTimeout}).Get("http://" + publicListener.Addr().String() + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck // test cleanup
	reader := bufio.NewReader(response.Body)
	if first, err := reader.ReadString('\n'); err != nil || first != "first\n" {
		t.Fatalf("first streamed chunk = %q, %v", first, err)
	}
	select {
	case <-time.After(150 * time.Millisecond):
	case <-runCtx.Done():
		t.Fatal(runCtx.Err())
	}
	releaseResponse()
	if second, err := reader.ReadString('\n'); err != nil || second != "second\n" {
		t.Fatalf("second streamed chunk = %q, %v", second, err)
	}
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestPublicServerRejectsOversizedHeadersBeforeHandler(t *testing.T) {
	unixListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publicListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		_ = unixListener.Close()
		t.Fatal(err)
	}
	handlerCalled := make(chan struct{}, 1)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveBoundListeners(
			runCtx, cancel,
			listenerConfig{
				webSocketPath: "/mesh", reporter: newErrorReporter(nil), shutdownTimeout: time.Second,
				publicHTTPHandler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { handlerCalled <- struct{}{} }),
			},
			func(context.Context, transport.Conn) error { return nil }, unixListener, nil, nil, publicListener,
		)
	}()
	connection, err := net.DialTimeout("tcp4", publicListener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close() //nolint:errcheck // test cleanup
	_ = connection.SetDeadline(time.Now().Add(runtimeTestTimeout))
	request := "GET / HTTP/1.1\r\nHost: app.shaulavo.dev\r\nX-Oversized: " + strings.Repeat("a", maximumPublicHeaderBytes+(8<<10)) + "\r\n\r\n"
	if _, err := io.WriteString(connection, request); err != nil {
		t.Fatal(err)
	}
	status, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil || !strings.Contains(status, " 431 ") {
		t.Fatalf("oversized-header response = %q, %v", status, err)
	}
	select {
	case <-handlerCalled:
		t.Fatal("oversized header reached the public handler")
	default:
	}
	cancel()
	if err := waitRuntime(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestBoundedPublicListenerCapsAndUnblocksAccepts(t *testing.T) {
	base := &queuedPublicListener{
		connections: make(chan net.Conn, 3), accepted: make(chan struct{}, 3), closed: make(chan struct{}),
	}
	peers := make([]net.Conn, 0, 3)
	for range 3 {
		server, peer := net.Pipe()
		base.connections <- server
		peers = append(peers, peer)
	}
	defer func() {
		for _, peer := range peers {
			_ = peer.Close()
		}
	}()
	listener := newBoundedPublicListener(base, 2)
	first, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	second, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		<-base.accepted
	}
	thirdResult := make(chan net.Conn, 1)
	thirdError := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		thirdResult <- connection
		thirdError <- acceptErr
	}()
	select {
	case <-base.accepted:
		t.Fatal("listener accepted beyond its connection cap")
	case <-time.After(25 * time.Millisecond):
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-base.accepted:
	case <-time.After(time.Second):
		t.Fatal("closing one connection did not admit one waiter")
	}
	third := <-thirdResult
	if err := <-thirdError; err != nil {
		t.Fatal(err)
	}
	fourthError := make(chan error, 1)
	go func() {
		_, acceptErr := listener.Accept()
		fourthError <- acceptErr
	}()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-fourthError:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("blocked Accept error = %v, want net.ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("listener shutdown did not unblock a capped Accept")
	}
	_ = second.Close()
	_ = third.Close()
	if err := listener.closeActive(); err != nil {
		t.Fatal(err)
	}
}

type queuedPublicListener struct {
	connections chan net.Conn
	accepted    chan struct{}
	closed      chan struct{}
	closeOnce   sync.Once
}

func (l *queuedPublicListener) Accept() (net.Conn, error) {
	l.accepted <- struct{}{}
	select {
	case connection := <-l.connections:
		return connection, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *queuedPublicListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (*queuedPublicListener) Addr() net.Addr { return &net.TCPAddr{} }

type failingListener struct {
	err error
}

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (failingListener) Close() error                { return nil }
func (failingListener) Addr() net.Addr              { return &net.UnixAddr{Name: "fake", Net: "unix"} }

type acceptThenFailListener struct {
	first chan net.Conn
	fail  chan struct{}
	err   error
}

func (l *acceptThenFailListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.first:
		return conn, nil
	default:
	}
	<-l.fail
	return nil, l.err
}

func (l *acceptThenFailListener) Close() error {
	select {
	case l.fail <- struct{}{}:
	default:
	}
	return nil
}

func (*acceptThenFailListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "fake", Net: "unix"}
}

func echoOneFrame(_ context.Context, conn transport.Conn) error {
	frame, err := conn.ReadFrame()
	if err != nil {
		return err
	}
	return conn.WriteFrame(frame)
}

func runRuntime(t *testing.T, ctx context.Context, cfg ListenerConfig, handler transport.Handler) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, cfg, handler)
	}()
	return done
}

func waitRuntime(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(runtimeTestTimeout):
		t.Fatal("runtime did not stop")
		return nil
	}
}

func dialUnixRuntime(t *testing.T, socketPath string) transport.Conn {
	t.Helper()
	deadline := time.Now().Add(runtimeTestTimeout)
	for {
		stream, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err == nil {
			conn, wrapErr := transport.NewStreamConn(stream)
			if wrapErr != nil {
				t.Fatal(wrapErr)
			}
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial daemon socket: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(runtimeTestTimeout)
	for {
		if _, err := os.Lstat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("path %s was not created", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(runtimeTestTimeout):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func reserveTCPPort(t *testing.T, address string) uint16 {
	t.Helper()
	listener, err := net.Listen("tcp4", net.JoinHostPort(address, "0"))
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port) //nolint:gosec // net.TCPAddr ports are bounded to uint16
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func waitForHTTPStatus(t *testing.T, url string, want int) {
	t.Helper()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(runtimeTestTimeout)
	for {
		response, err := client.Get(url)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s did not return %d: %v", url, want, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForTCPRuntime(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(runtimeTestTimeout)
	for {
		connection, err := net.DialTimeout("tcp4", address, 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("TCP listener %s did not start: %v", address, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertHTTPSCertificateAndRoute(t *testing.T, port uint16, certificatePEM []byte, serial int64) {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificatePEM) {
		t.Fatal("append HTTPS test root")
	}
	address := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "probe.mesh.shaulavo.dev"}
	connection, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	if got := connection.ConnectionState().PeerCertificates[0].SerialNumber; got.Cmp(big.NewInt(serial)) != 0 {
		_ = connection.Close()
		t.Fatalf("HTTPS certificate serial = %s, want %d", got, serial)
	}
	_ = connection.Close()

	transport := &http.Transport{
		TLSClientConfig: tlsConfig.Clone(),
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp4", address)
		},
	}
	client := &http.Client{Transport: transport, Timeout: time.Second}
	response, err := client.Get(fmt.Sprintf("https://probe.mesh.shaulavo.dev:%d/service", port))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Mesh-Route") != "service" {
		t.Fatalf("HTTPS /service status = %d, route = %q", response.StatusCode, response.Header.Get("X-Mesh-Route"))
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "/service" {
		t.Fatalf("HTTPS /service body = %q, want service path", body)
	}
	_ = response.Body.Close()

	response, err = client.Get(fmt.Sprintf("https://probe.mesh.shaulavo.dev:%d/mesh", port))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound || response.Header.Get("X-Mesh-Route") != "" {
		_ = response.Body.Close()
		t.Fatalf("HTTPS reserved /mesh status = %d, route = %q", response.StatusCode, response.Header.Get("X-Mesh-Route"))
	}
	_ = response.Body.Close()

	response, err = client.Get(fmt.Sprintf("https://probe.mesh.shaulavo.dev:%d/m%%65sh", port))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound || response.Header.Get("X-Mesh-Route") != "" {
		_ = response.Body.Close()
		t.Fatalf("HTTPS encoded reserved path status = %d, route = %q", response.StatusCode, response.Header.Get("X-Mesh-Route"))
	}
	_ = response.Body.Close()
	transport.CloseIdleConnections()
}

func daemonTestCertificate(t *testing.T, serial int64, now time.Time) ([]byte, []byte) {
	return daemonTestNamedCertificate(t, serial, now, dnsname.WildcardName)
}

func daemonTestNamedCertificate(t *testing.T, serial int64, now time.Time, name string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func assertRuntimeFrame(t *testing.T, got, want protocol.Frame) {
	t.Helper()
	if got.Kind != want.Kind || got.Session != want.Session || got.Seq != want.Seq || string(got.Payload) != string(want.Payload) {
		t.Fatalf("frame = %#v, want %#v", got, want)
	}
}
