package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	meshdaemon "github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestExplicitSessionIntentWakesAnUnavailableHost(t *testing.T) {
	for _, args := range [][]string{{"pc", "--", "/bin/true"}, {"pc", "-r"}, {"7K3D"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			testWakeSessionIntent(t, args)
		})
	}
}

func testWakeSessionIntent(t *testing.T, args []string) {
	t.Helper()
	host := setupCommandTestHost(t)
	saveWakeSessionCache(t, host)
	awake, wakes := false, 0
	_, _, err := executeCommand(t, Dependencies{
		DialHost: func(ctx context.Context, target HostRecord) (transport.Conn, error) {
			if !awake {
				return nil, errors.New("host unavailable")
			}
			return host.dial(ctx, target)
		},
		Wake: func(context.Context, HostRecord) error {
			wakes++
			awake = true
			return nil
		},
	}, args...)
	if err != nil {
		t.Fatal(err)
	}
	if wakes != 1 || host.eventCount(protocol.TypeAttach) != 1 {
		t.Fatalf("wake calls=%d, events=%v", wakes, host.recorded())
	}
	wantCreates := 0
	if len(args) > 2 {
		wantCreates = 1
	}
	if host.eventCount(protocol.TypeCreate) != wantCreates {
		t.Fatalf("create calls=%d, want %d", host.eventCount(protocol.TypeCreate), wantCreates)
	}
}

func TestCatalogPollingNeverWakesUnavailableHosts(t *testing.T) {
	host := setupCommandTestHost(t)
	saveWakeSessionCache(t, host)
	wakes := 0
	dependencies := Dependencies{
		DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
			return nil, errors.New("host unavailable")
		},
		Wake: func(context.Context, HostRecord) error { wakes++; return nil },
	}
	for range 3 {
		_, _, err := executeCommand(t, dependencies, "ls", "--timeout", "20ms")
		if err != nil {
			t.Fatal(err)
		}
	}
	if wakes != 0 {
		t.Fatalf("catalog polling woke the host %d times", wakes)
	}
}

func TestFailedSessionCreationDoesNotWakeOrRetryTheCommand(t *testing.T) {
	host := setupCommandTestHost(t)
	host.createError = "worker startup failed"
	wakes := 0
	_, _, err := executeCommand(t, Dependencies{
		DialHost: host.dial,
		Wake:     func(context.Context, HostRecord) error { wakes++; return nil },
	}, "pc", "--", "/bin/true")
	if err == nil || !strings.Contains(err.Error(), host.createError) {
		t.Fatalf("create error=%v", err)
	}
	if wakes != 0 || host.eventCount(protocol.TypeCreate) != 1 {
		t.Fatalf("wake calls=%d, events=%v", wakes, host.recorded())
	}
}

func TestColdBootResumeDoesNotCreateANewSession(t *testing.T) {
	host := setupCommandTestHost(t)
	awake, wakes := false, 0
	_, _, err := executeCommand(t, Dependencies{
		DialHost: func(ctx context.Context, target HostRecord) (transport.Conn, error) {
			if !awake {
				return nil, errors.New("host unavailable")
			}
			return host.dial(ctx, target)
		},
		Wake: func(context.Context, HostRecord) error {
			wakes++
			awake = true
			host.sessionState = "interrupted"
			return nil
		},
	}, "pc", "-r")
	if err == nil || !strings.Contains(err.Error(), "no active sessions") {
		t.Fatalf("resume error=%v", err)
	}
	if wakes != 1 || host.eventCount(protocol.TypeCreate) != 0 || host.eventCount(protocol.TypeAttach) != 0 {
		t.Fatalf("wake calls=%d, events=%v", wakes, host.recorded())
	}
}

func TestWakePermissionCommandsOnlyConfigureTheLocalDaemon(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "deny", true: "allow"}[allowed], func(t *testing.T) {
			testWakePermissionCommand(t, allowed)
		})
	}
}

func testWakePermissionCommand(t *testing.T, allowed bool) {
	t.Helper()
	stateDir, err := os.MkdirTemp("", "mesh-wake-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stateDir) //nolint:errcheck // test resource cleanup
	t.Setenv("MESH_STATE_DIR", stateDir)
	listener, err := net.Listen("unix", meshdaemon.SocketPath(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck // test resource cleanup
	type result struct {
		request protocol.Control
		err     error
	}
	done := make(chan result, 1)
	go func() {
		request, err := serveWakePermissionRequest(listener)
		done <- result{request, err}
	}()
	verb := map[bool]string{false: "deny", true: "allow"}[allowed]
	stdout, _, err := executeCommand(t, Dependencies{
		DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
			t.Error("wake permission command dialed a remote host")
			return nil, errors.New("unexpected remote dial")
		},
	}, "wake", verb)
	if err != nil {
		t.Fatal(err)
	}
	response := <-done
	if response.err != nil {
		t.Fatal(response.err)
	}
	if response.request.Type != protocol.TypeWakeConfigure || response.request.WakeAllowed == nil || *response.request.WakeAllowed != allowed {
		t.Fatalf("wake permission request = %#v", response.request)
	}
	want := "Wake permission disabled on this host"
	if allowed {
		want = "Wake permission saved; waking is unavailable until a wired network is discovered"
	}
	if !strings.Contains(stdout, want) {
		t.Fatalf("wake %s output = %q, want %q", verb, stdout, want)
	}
}

func serveWakePermissionRequest(listener net.Listener) (protocol.Control, error) {
	stream, err := listener.Accept()
	if err != nil {
		return protocol.Control{}, err
	}
	defer stream.Close() //nolint:errcheck // test resource cleanup
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		return protocol.Control{}, err
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return protocol.Control{}, err
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return protocol.Control{}, err
	}
	return request, conn.WriteFrame(mustCommandControlFrame(protocol.Control{
		Type: protocol.TypeWakeConfigured, RequestID: request.RequestID,
	}))
}

func saveWakeSessionCache(t *testing.T, host *commandTestHost) {
	t.Helper()
	cache, err := OpenCatalogCache(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close() //nolint:errcheck // test resource cleanup
	if err := cache.Save(context.Background(), host.host, []protocol.SessionInfo{{
		ID: "7K3D", HostID: host.host.ID, State: "running", Command: []string{"bash"}, CreatedAt: commandTestTime,
	}}); err != nil {
		t.Fatal(err)
	}
}
