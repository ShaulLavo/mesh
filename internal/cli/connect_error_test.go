package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/transport"
)

func connectErrorTestHost(endpoint string) HostRecord {
	return HostRecord{MachineName: "pc", ID: "pc-id", Endpoint: endpoint}
}

// wrappedDialError reproduces the chain a sandboxed WebSocket dial used to
// return: WebSocket and HTTP wrapping around *net.OpError and *os.SyscallError.
func wrappedDialError(resolved string, errno syscall.Errno) error {
	addr, err := net.ResolveTCPAddr("tcp", resolved)
	if err != nil {
		panic(err)
	}
	dial := &net.OpError{Op: "dial", Net: "tcp", Addr: addr, Err: os.NewSyscallError("connect", errno)}
	get := fmt.Errorf("Get %q: %w", "http://"+resolved+"/mesh", dial)
	return fmt.Errorf("transport: dial ws://%s/mesh: failed to WebSocket dial: failed to send handshake request: %w", resolved, get)
}

func failingDialer(err error) HostDialer {
	return func(context.Context, HostRecord) (transport.Conn, error) { return nil, err }
}

// assertConnectionFailure checks the presented message and that the original
// errno survives for callers that branch on it.
func assertConnectionFailure(t *testing.T, err error, want string, errno syscall.Errno) {
	t.Helper()
	if err == nil {
		t.Fatal("dial failure was accepted")
	}
	if err.Error() != want {
		t.Fatalf("error = %q\nwant    %q", err, want)
	}
	if !errors.Is(err, errno) {
		t.Fatalf("error %q lost its cause %v", err, errno)
	}
}

func TestControlDialFailuresBeforeAuthenticationAreConnectionErrors(t *testing.T) {
	const resolved = "[fd7a:115c:a1e0::5e36:5e5f]:7337"
	cases := []struct {
		name  string
		errno syscall.Errno
		want  string
	}{
		{"permission", syscall.EPERM, "Cannot connect to pc: TCP connection to " + resolved + " was blocked by local permissions (operation not permitted). If this shell runs in a sandbox, such as Codex workspace-write, turn on its network access."},
		{"access", syscall.EACCES, "Cannot connect to pc: TCP connection to " + resolved + " was blocked by local permissions (permission denied). If this shell runs in a sandbox, such as Codex workspace-write, turn on its network access."},
		{"refused", syscall.ECONNREFUSED, "Cannot connect to pc: nothing accepted the TCP connection to " + resolved + " (connection refused). Check that Mesh is running on pc."},
		{"timeout", syscall.ETIMEDOUT, "Cannot connect to pc: TCP connection to " + resolved + " got no answer (" + syscall.ETIMEDOUT.Error() + "). Check that pc is online in Tailscale."},
		{"no route", syscall.EHOSTUNREACH, "Cannot connect to pc: no network route to " + resolved + " (" + syscall.EHOSTUNREACH.Error() + "). Check that pc is online in Tailscale."},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			// The endpoint names a host; the message must print the address
			// the dial actually tried.
			host := connectErrorTestHost("ws://pc.example.ts.net:7337/mesh")
			cause := fmt.Errorf("authenticate control host: %w", wrappedDialError(resolved, test.errno))
			_, _, err := openVerifiedHostInfo(context.Background(), host, failingDialer(cause))
			assertConnectionFailure(t, err, test.want, test.errno)
		})
	}
}

func TestDialTimeoutWithoutErrnoIsAConnectionError(t *testing.T) {
	host := connectErrorTestHost("ws://100.64.0.2:7337/mesh")
	addr := &net.TCPAddr{IP: net.ParseIP("100.64.0.2"), Port: 7337}
	cause := fmt.Errorf("transport: dial: %w", &net.OpError{Op: "dial", Net: "tcp", Addr: addr, Err: timeoutError{}})
	err := classifyConnectError(host, endpointAddress(host), cause)
	if err == nil || err.Error() != "Cannot connect to pc: TCP connection to 100.64.0.2:7337 got no answer (i/o timeout). Check that pc is online in Tailscale." {
		t.Fatalf("error = %v", err)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestCanceledDialIsNotAConnectionError(t *testing.T) {
	host := connectErrorTestHost("ws://100.64.0.2:7337/mesh")
	cause := &net.OpError{Op: "dial", Net: "tcp", Err: context.Canceled}
	if err := classifyConnectError(host, endpointAddress(host), cause); err != nil {
		t.Fatalf("canceled dial classified as %q", err)
	}
}

func TestWakeJoinKeepsEveryDialCause(t *testing.T) {
	refused := wrappedDialError("100.64.0.2:7337", syscall.ECONNREFUSED)
	blocked := fmt.Errorf("wake host pc: %w", wrappedDialError("100.64.0.9:7338", syscall.EPERM))
	app := &application{dependencies: Dependencies{
		DialHost: failingDialer(refused),
		Wake:     func(context.Context, HostRecord) error { return blocked },
	}}
	host := connectErrorTestHost("ws://100.64.0.2:7337/mesh")
	_, _, err := openVerifiedHostInfo(context.Background(), host, app.intentDialer(io.Discard))
	if !errors.Is(err, syscall.ECONNREFUSED) || !errors.Is(err, syscall.EPERM) {
		t.Fatalf("joined error %q lost a cause", err)
	}
	for _, want := range []string{
		"Cannot connect to pc: nothing accepted the TCP connection to 100.64.0.2:7337 (connection refused).",
		"Cannot connect to pc: TCP connection to 100.64.0.9:7338 was blocked by local permissions (operation not permitted).",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q is missing %q", err, want)
		}
	}
}

var errWakeSenderRefused = errors.New("wake sender refused")

func TestWakeJoinSanitizesRemoteWakeMessage(t *testing.T) {
	remote := fmt.Errorf("wake through vps: spoofed\r\x1b[31mline: %w", errWakeSenderRefused)
	app := &application{dependencies: Dependencies{
		DialHost: failingDialer(wrappedDialError("100.64.0.2:7337", syscall.ECONNREFUSED)),
		Wake:     func(context.Context, HostRecord) error { return remote },
	}}
	host := connectErrorTestHost("ws://100.64.0.2:7337/mesh")
	_, _, err := openVerifiedHostInfo(context.Background(), host, app.intentDialer(io.Discard))
	if !errors.Is(err, syscall.ECONNREFUSED) || !errors.Is(err, errWakeSenderRefused) {
		t.Fatalf("joined error %q lost a cause", err)
	}
	if strings.ContainsAny(err.Error(), "\r\x1b") {
		t.Fatalf("remote control characters reached the error: %q", err)
	}
	for _, want := range []string{
		"Cannot connect to pc: nothing accepted the TCP connection to 100.64.0.2:7337 (connection refused).",
		"wake through vps: spoofed [31mline: wake sender refused",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q is missing %q", err, want)
		}
	}
}

func closedLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func refusedMessage(address string) string {
	return "Cannot connect to pc: nothing accepted the TCP connection to " + address + " (connection refused). Check that Mesh is running on pc."
}

func TestControlDialToClosedPortReportsRefusedConnection(t *testing.T) {
	prepareCommandEnvironment(t)
	_, hostID := controlFixtureAuthentication(t)
	address := closedLoopbackAddress(t)
	host := connectErrorTestHost("ws://" + address + "/mesh")
	host.MeshIdentity = hostID
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := dialControlHost(ctx, host)
	assertConnectionFailure(t, err, refusedMessage(address), syscall.ECONNREFUSED)
}

func TestPassiveDialToClosedPortKeepsMCPRetry(t *testing.T) {
	prepareCommandEnvironment(t)
	_, hostID := controlFixtureAuthentication(t)
	address := closedLoopbackAddress(t)
	host := connectErrorTestHost("ws://" + address + "/mesh")
	host.MeshIdentity = hostID
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := dialAgentHost(ctx, host)
	assertConnectionFailure(t, err, refusedMessage(address), syscall.ECONNREFUSED)
	if code, retryable := classifyMCPTransportError(err); code != agentapi.HostUnreachable || !retryable {
		t.Fatalf("MCP classification = %s retryable=%t, want %s retryable", code, retryable, agentapi.HostUnreachable)
	}
}

func TestControlDialDeadlineBeforeUpgradeReportsTimeout(t *testing.T) {
	prepareCommandEnvironment(t)
	_, hostID := controlFixtureAuthentication(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	go func() { defer close(held); holdSilentConnections(listener) }()
	defer func() { _ = listener.Close(); <-held }()
	host := connectErrorTestHost("ws://" + listener.Addr().String() + "/mesh")
	host.MeshIdentity = hostID
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = dialControlHost(ctx, host)
	want := "Cannot connect to pc: control connection to " + listener.Addr().String() + " timed out before Mesh authentication (context deadline exceeded). Check that pc is online in Tailscale."
	if err == nil || err.Error() != want || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v\nwant    %q", err, want)
	}
}

// holdSilentConnections accepts TCP and never answers, so the HTTP upgrade
// waits until the caller's deadline.
func holdSilentConnections(listener net.Listener) {
	var held []net.Conn
	defer func() {
		for _, conn := range held {
			_ = conn.Close()
		}
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		held = append(held, conn)
	}
}

func TestSSHDialToClosedPortReportsRefusedConnection(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	address := closedLoopbackAddress(t)
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	host := connectErrorTestHost("ws://127.0.0.1:7337/mesh")
	host.MeshIdentity = base64.RawURLEncoding.EncodeToString(public)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = dialMeshSSHWithKey(ctx, host, key, uint16(portNumber))
	assertConnectionFailure(t, err, refusedMessage(address), syscall.ECONNREFUSED)
}

func TestControlAuthenticationRejectionStaysAnAuthenticationError(t *testing.T) {
	prepareCommandEnvironment(t)
	auth, hostID := controlFixtureAuthentication(t)
	auth.Authorize = func(string) bool { return false }
	served := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, func(context.Context, transport.Conn) error {
			served <- struct{}{}
			return nil
		})
	}))
	defer server.Close()
	host := connectErrorTestHost("ws" + strings.TrimPrefix(server.URL, "http") + "/mesh")
	host.MeshIdentity = hostID
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := dialControlHost(ctx, host)
	if err == nil {
		// A rejected peer may only learn of the refusal on its first read.
		_, err = conn.ReadFrame()
		_ = conn.Close()
	}
	assertAuthenticationFailure(t, err)
	select {
	case <-served:
		t.Fatal("unauthorized device reached the control handler")
	default:
	}
}

func assertAuthenticationFailure(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("unauthorized device reached the control handler")
	}
	var connectErr *hostConnectError
	if errors.As(err, &connectErr) {
		t.Fatalf("authentication rejection reported as a connection failure: %q", err)
	}
	if !errors.Is(err, transport.ErrAuthentication) || !strings.HasPrefix(err.Error(), "authenticate control host: ") {
		t.Fatalf("error %q is not reported as an authentication failure", err)
	}
}
