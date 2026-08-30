package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

var commandTestTime = time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)

type commandTestHost struct {
	host         HostRecord
	sessionState string
	mu           sync.Mutex
	events       []string
}

func (h *commandTestHost) dial(context.Context, HostRecord) (transport.Conn, error) {
	return &commandTestConn{host: h, responses: make(chan protocol.Frame, 8)}, nil
}

func (h *commandTestHost) record(event string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
}

func (h *commandTestHost) recorded() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.events)
}

type commandTestConn struct {
	host      *commandTestHost
	responses chan protocol.Frame
}

func (c *commandTestConn) ReadFrame() (protocol.Frame, error) {
	response, ok := <-c.responses
	if !ok {
		return protocol.Frame{}, io.EOF
	}
	return response, nil
}

func (c *commandTestConn) WriteFrame(frame protocol.Frame) error {
	if frame.Kind != protocol.KindControl {
		return nil
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return err
	}
	c.host.record(request.Type)
	response := protocol.Control{RequestID: request.RequestID, SessionID: request.SessionID}
	switch request.Type {
	case protocol.TypeHostInfo:
		response.Type = protocol.TypeHostInfoResult
		response.Host = &protocol.HostInfo{ID: c.host.host.ID, MeshIdentity: c.host.host.MeshIdentity, TailscaleName: c.host.host.TailscaleName}
	case protocol.TypeList:
		response.Type = protocol.TypeListed
		state := c.host.sessionState
		if state == "" {
			state = "running"
		}
		var exitCode *int
		if state == "exited" {
			code := 0
			exitCode = &code
		}
		response.Sessions = []protocol.SessionInfo{{
			ID: "7K3D", HostID: c.host.host.ID, Command: []string{"bash"}, Cwd: "/work",
			State: state, CreatedAt: commandTestTime, ExitCode: exitCode, LastOutputSequence: 64,
		}}
	case protocol.TypeCreate:
		response.Type = protocol.TypeCreated
		response.SessionID = "7K3D"
	case protocol.TypeAttach:
		response.Type = protocol.TypeAttached
		response.Seq = 0
		c.responses <- mustCommandControlFrame(response)
		exitCode := 0
		c.responses <- mustCommandControlFrame(protocol.Control{Type: protocol.TypeExit, SessionID: request.SessionID, ExitCode: &exitCode})
		return nil
	case protocol.TypeKill:
		response.Type = protocol.TypeOK
	case protocol.TypeLogs:
		response.Type = protocol.TypeLogged
		response.Output = []byte("recent remote output\r\n")
	default:
		return errors.New("unexpected command test control " + request.Type)
	}
	c.responses <- mustCommandControlFrame(response)
	return nil
}

func (c *commandTestConn) Close() error { return nil }

func mustCommandControlFrame(message protocol.Control) protocol.Frame {
	payload, err := message.Encode()
	if err != nil {
		panic(err)
	}
	return protocol.Frame{Kind: protocol.KindControl, Payload: payload}
}

func TestRootDispatchesHostAndSessionTargets(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantEvents []string
	}{
		{
			name:       "host creates then attaches",
			args:       []string{"pc", "--", "/bin/true"},
			wantEvents: []string{protocol.TypeHostInfo, protocol.TypeCreate, protocol.TypeHostInfo, protocol.TypeAttach},
		},
		{
			name:       "session lists then attaches",
			args:       []string{"7K3D"},
			wantEvents: []string{protocol.TypeHostInfo, protocol.TypeList, protocol.TypeHostInfo, protocol.TypeAttach},
		},
		{
			name:       "resume flag after host lists then attaches",
			args:       []string{"pc", "-r"},
			wantEvents: []string{protocol.TypeHostInfo, protocol.TypeList, protocol.TypeHostInfo, protocol.TypeAttach},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := setupCommandTestHost(t)
			stdout, _, err := executeCommand(t, Dependencies{DialHost: host.dial, Now: func() time.Time { return commandTestTime }}, test.args...)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(stdout, "recent") {
				t.Fatalf("unexpected command output %q", stdout)
			}
			if got := host.recorded(); !slices.Equal(got, test.wantEvents) {
				t.Fatalf("events = %v, want %v", got, test.wantEvents)
			}
		})
	}
}

func TestListCommandReturnsCachedRowsAsStale(t *testing.T) {
	host := setupCommandTestHost(t)
	cache, err := OpenCatalogCache(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Save(context.Background(), host.host, []protocol.SessionInfo{{
		ID: "7K3D", HostID: host.host.ID, Command: []string{"bash"}, Cwd: "/work",
		State: "running", CreatedAt: commandTestTime, LastOutputSequence: 64,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := executeCommand(t, Dependencies{
		DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
			return nil, errors.New("host is offline")
		},
		Now: func() time.Time { return commandTestTime.Add(time.Minute) },
	}, "ls", "--timeout", "20ms")
	if err != nil {
		t.Fatalf("ls returned an error: %v", err)
	}
	if !strings.Contains(stdout, "7K3D") || !strings.Contains(stdout, "stale") {
		t.Fatalf("ls output = %q, want cached stale row", stdout)
	}
	if !strings.Contains(stderr, "pc: unavailable") {
		t.Fatalf("ls stderr = %q, want offline diagnostic", stderr)
	}
}

func TestKillAndLogsRouteToTheResolvedRemoteHost(t *testing.T) {
	host := setupCommandTestHost(t)
	stdout, _, err := executeCommand(t, Dependencies{DialHost: host.dial, Now: func() time.Time { return commandTestTime }}, "kill", "7K3D")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "killed 7K3D") {
		t.Fatalf("kill output = %q", stdout)
	}

	stdout, _, err = executeCommand(t, Dependencies{DialHost: host.dial, Now: func() time.Time { return commandTestTime }}, "logs", "7K3D", "--tail", "1024")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "recent remote output\r\n" {
		t.Fatalf("logs output = %q", stdout)
	}
	events := host.recorded()
	if !slices.Contains(events, protocol.TypeKill) || !slices.Contains(events, protocol.TypeLogs) {
		t.Fatalf("control events = %v, want kill and logs", events)
	}
}

func TestAddSavesVerifiedHostAndReportsConvergedRerun(t *testing.T) {
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	bootstrapCalls := 0
	bootstrapHost := HostRecord{
		ID: "host-id", MeshIdentity: "mesh-identity", TailscaleName: "pc.example.ts.net",
		Addresses: []string{"100.64.0.2"}, Endpoint: "ws://100.64.0.2:7337/mesh",
	}
	bootstrap := func(_ context.Context, request AddRequest) (BootstrapResult, error) {
		bootstrapCalls++
		if request.Target != "alice@pc.example.ts.net" || request.Alias != "pc" {
			t.Fatalf("add request = %#v", request)
		}
		return BootstrapResult{Host: bootstrapHost, AlreadyConfigured: bootstrapCalls > 1}, nil
	}

	stdout, _, err := executeCommand(t, Dependencies{Bootstrap: bootstrap}, "add", "alice@pc.example.ts.net")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "added pc (host-id)") {
		t.Fatalf("first add output = %q", stdout)
	}
	stdout, _, err = executeCommand(t, Dependencies{Bootstrap: bootstrap}, "add", "alice@pc.example.ts.net")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "already configured pc (host-id)") {
		t.Fatalf("second add output = %q", stdout)
	}
	hosts, err := LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Alias != "pc" || hosts[0].Endpoint != bootstrapHost.Endpoint {
		t.Fatalf("saved hosts = %#v", hosts)
	}
}

func TestLogsRoutesCatalogedExitedSessionToRemoteFallback(t *testing.T) {
	host := setupCommandTestHost(t)
	host.sessionState = "exited"
	stdout, _, err := executeCommand(t, Dependencies{DialHost: host.dial}, "logs", "7K3D", "--tail", "1024")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "recent remote output\r\n" {
		t.Fatalf("logs output = %q", stdout)
	}
	if !slices.Contains(host.recorded(), protocol.TypeLogs) {
		t.Fatalf("events = %v, want logs", host.recorded())
	}
}

func TestPickerWakeRefreshesAndRejectsImpossibleSelections(t *testing.T) {
	host := setupCommandTestHost(t)
	for _, invalid := range []PickerSelection{
		{SessionID: "7K3D", New: true},
		{SessionID: "7K3D", Wake: true},
		{HostAlias: "pc", New: true, Wake: true},
		{Wake: true},
	} {
		if err := validatePickerSelection(invalid); err == nil {
			t.Errorf("selection %+v was accepted", invalid)
		}
	}

	pickerCalls := 0
	wakeCalls := 0
	_, stderr, err := executeCommand(t, Dependencies{
		DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
			return nil, errors.New("host is asleep")
		},
		Picker: func(_ context.Context, input PickerInput) (PickerSelection, error) {
			pickerCalls++
			if len(input.Hosts) != 1 || input.Hosts[0].Host.Alias != "pc" {
				t.Fatalf("picker input = %#v", input)
			}
			if pickerCalls == 1 {
				return PickerSelection{HostAlias: "pc", Wake: true}, nil
			}
			return PickerSelection{}, nil
		},
		Wake: func(_ context.Context, selected HostRecord) error {
			wakeCalls++
			if selected.ID != host.host.ID {
				t.Fatalf("wake host = %#v", selected)
			}
			return nil
		},
	}, []string{}...)
	if err != nil {
		t.Fatal(err)
	}
	if pickerCalls != 2 || wakeCalls != 1 {
		t.Fatalf("picker calls = %d, wake calls = %d; want 2 and 1", pickerCalls, wakeCalls)
	}
	if !strings.Contains(stderr, "woke pc; refreshing hosts") {
		t.Fatalf("picker stderr = %q", stderr)
	}
}

func setupCommandTestHost(t *testing.T) *commandTestHost {
	t.Helper()
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	t.Setenv("MESH_STATE_DIR", t.TempDir())
	host := &commandTestHost{host: HostRecord{
		Alias: "pc", ID: "host-id", MeshIdentity: "host-key", TailscaleName: "pc.example.ts.net",
		Addresses: []string{"100.64.0.2"}, Endpoint: "ws://100.64.0.2:7777/mesh",
	}}
	if err := SaveHost(host.host); err != nil {
		t.Fatal(err)
	}
	return host
}

func executeCommand(t *testing.T, dependencies Dependencies, args ...string) (string, string, error) {
	t.Helper()
	stdin, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	defer stdout.Close()
	defer stderr.Close()
	dependencies.Stdin = stdin
	dependencies.Stdout = stdout
	dependencies.Stderr = stderr
	command := NewCommand(dependencies)
	command.SetArgs(args)
	err = command.ExecuteContext(context.Background())
	return readCommandFile(t, stdout), readCommandFile(t, stderr), err
}

func readCommandFile(t *testing.T, file *os.File) string {
	t.Helper()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}
