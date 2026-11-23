package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/transport"
)

func connectErrorTestHost(endpoint string) HostRecord {
	return HostRecord{MachineName: "pc", ID: "pc-id", Endpoint: endpoint}
}

// wrappedDialError reproduces the chain a sandboxed WebSocket dial returns:
// transport and WebSocket wrapping around *net.OpError and *os.SyscallError.
func wrappedDialError(address string, errno syscall.Errno) error {
	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		panic(err)
	}
	dial := &net.OpError{Op: "dial", Net: "tcp", Addr: addr, Err: os.NewSyscallError("connect", errno)}
	get := fmt.Errorf("Get %q: %w", "http://"+address+"/mesh", dial)
	return fmt.Errorf("transport: dial ws://%s/mesh: failed to WebSocket dial: failed to send handshake request: %w", address, get)
}

func TestControlDialFailuresBeforeAuthenticationAreConnectionErrors(t *testing.T) {
	cases := []struct {
		name  string
		errno syscall.Errno
		want  string
	}{
		{"permission", syscall.EPERM, "Cannot connect to pc: TCP connection to [fd7a:115c:a1e0::5e36:5e5f]:7337 was blocked by local permissions (operation not permitted). If this shell runs in a sandbox, such as Codex workspace-write, turn on its network access."},
		{"access", syscall.EACCES, "Cannot connect to pc: TCP connection to [fd7a:115c:a1e0::5e36:5e5f]:7337 was blocked by local permissions (permission denied). If this shell runs in a sandbox, such as Codex workspace-write, turn on its network access."},
		{"refused", syscall.ECONNREFUSED, "Cannot connect to pc: nothing accepted the TCP connection to [fd7a:115c:a1e0::5e36:5e5f]:7337 (connection refused). Check that Mesh is running on pc."},
		{"timeout", syscall.ETIMEDOUT, "Cannot connect to pc: TCP connection to [fd7a:115c:a1e0::5e36:5e5f]:7337 got no answer (" + syscall.ETIMEDOUT.Error() + "). Check that pc is online in Tailscale."},
		{"no route", syscall.EHOSTUNREACH, "Cannot connect to pc: no network route to [fd7a:115c:a1e0::5e36:5e5f]:7337 (" + syscall.EHOSTUNREACH.Error() + "). Check that pc is online in Tailscale."},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			host := connectErrorTestHost("ws://[fd7a:115c:a1e0::5e36:5e5f]:7337/mesh")
			cause := fmt.Errorf("authenticate control host: %w", wrappedDialError("[fd7a:115c:a1e0::5e36:5e5f]:7337", test.errno))
			dial := func(context.Context, HostRecord) (transport.Conn, error) { return nil, cause }
			_, _, err := openVerifiedHostInfo(context.Background(), host, dial)
			if err == nil {
				t.Fatal("dial failure was accepted")
			}
			if got := err.Error(); !strings.HasPrefix(got, test.want) {
				t.Fatalf("error = %q, want prefix %q", got, test.want)
			}
			if !errors.Is(err, test.errno) {
				t.Fatalf("error %q lost its cause %v", err, test.errno)
			}
			if strings.Contains(strings.ToLower(strings.TrimPrefix(err.Error(), test.want)), "authenticat") {
				t.Fatalf("connection failure reported as authentication: %q", err)
			}
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

func TestControlDialToClosedPortReportsRefusedConnection(t *testing.T) {
	prepareCommandEnvironment(t)
	_, hostID := controlFixtureAuthentication(t)
	address := closedLoopbackAddress(t)
	host := connectErrorTestHost("ws://" + address + "/mesh")
	host.MeshIdentity = hostID
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := dialControlHost(ctx, host)
	want := "Cannot connect to pc: nothing accepted the TCP connection to " + address + " (connection refused). Check that Mesh is running on pc."
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("error %q lost ECONNREFUSED", err)
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
	var portNumber uint16
	if _, err := fmt.Sscan(port, &portNumber); err != nil {
		t.Fatal(err)
	}
	host := connectErrorTestHost("ws://127.0.0.1:7337/mesh")
	host.MeshIdentity = base64.RawURLEncoding.EncodeToString(public)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = dialMeshSSHWithKey(ctx, host, key, portNumber)
	want := "Cannot connect to pc: nothing accepted the TCP connection to " + address + " (connection refused). Check that Mesh is running on pc."
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
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
	select {
	case <-served:
		t.Fatal("unauthorized device reached the control handler")
	default:
	}
}
