package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

var commandTestTime = time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)

type commandTestHost struct {
	host         HostRecord
	sessionState string
	attachStart  func()
	mu           sync.Mutex
	events       []string
	services     []protocol.ServiceInfo
	previewRoot  string
	previewFiles uint64
	edgeRoutes   []protocol.EdgeRouteInfo
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
		response.Host = &protocol.HostInfo{
			ID: c.host.host.ID, MeshIdentity: c.host.host.MeshIdentity, TailscaleName: c.host.host.TailscaleName,
			PrivateName: "pc.mesh.shaulavo.dev",
		}
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
		if c.host.attachStart != nil {
			c.host.attachStart()
		}
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
	case protocol.TypeServicePreview:
		preview := *request.Service
		if numericCLIServiceTarget(preview.Target) && preview.Kind == "" {
			preview.Kind = "proxy"
			port, _ := strconv.ParseUint(preview.Target, 10, 16)
			preview.Target = strconv.FormatUint(port, 10)
		} else {
			if preview.Kind == "" {
				preview.Kind = "static"
			}
			if c.host.previewRoot != "" {
				preview.Target = c.host.previewRoot
			} else if !filepath.IsAbs(preview.Target) {
				preview.Target = filepath.Join("/home/test", preview.Target)
			}
		}
		response.Type = protocol.TypeServicePreviewed
		response.ServicePreview = &protocol.ServicePreview{Service: preview, FileCount: c.host.previewFiles}
	case protocol.TypeServiceUpsert:
		if request.ServicePreview == nil {
			return errors.New("service upsert omitted preview")
		}
		service := request.ServicePreview.Service
		service.Healthy = true
		c.host.mu.Lock()
		replaced := false
		for index := range c.host.services {
			if c.host.services[index].Name == service.Name {
				c.host.services[index] = service
				replaced = true
			}
		}
		if !replaced {
			c.host.services = append(c.host.services, service)
		}
		sort.Slice(c.host.services, func(i, j int) bool { return c.host.services[i].Name < c.host.services[j].Name })
		c.host.mu.Unlock()
		response.Type = protocol.TypeServiceUpserted
		response.Service = &service
	case protocol.TypeServiceList:
		c.host.mu.Lock()
		response.Services = append([]protocol.ServiceInfo(nil), c.host.services...)
		c.host.mu.Unlock()
		response.Type = protocol.TypeServiceListed
	case protocol.TypeServiceDelete:
		c.host.mu.Lock()
		remaining := c.host.services[:0]
		for _, service := range c.host.services {
			if service.Name != request.ServiceName {
				remaining = append(remaining, service)
			}
		}
		c.host.services = remaining
		c.host.mu.Unlock()
		response.Type = protocol.TypeServiceDeleted
		response.ServiceName = request.ServiceName
	case protocol.TypeEdgeList:
		c.host.mu.Lock()
		response.EdgeRoutes = append([]protocol.EdgeRouteInfo(nil), c.host.edgeRoutes...)
		c.host.mu.Unlock()
		response.Type = protocol.TypeEdgeListed
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

func TestServeCommandPreviewsThenPublishesCanonicalService(t *testing.T) {
	host := setupCommandTestHost(t)
	stdout, _, err := executeCommand(t, Dependencies{DialControl: host.dial}, "serve", "pc", "03000", "--at", "/api")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "serving https://pc.mesh.shaulavo.dev/api on pc (proxy -> 3000)") {
		t.Fatalf("serve output = %q", stdout)
	}
	if got := host.recorded(); !slices.Equal(got, []string{
		protocol.TypeHostInfo, protocol.TypeServicePreview,
		protocol.TypeHostInfo, protocol.TypeServiceUpsert,
	}) {
		t.Fatalf("serve events = %v", got)
	}
}

func TestPublicServeConfirmationUsesRemoteFactsAndYesOnlySkipsPrompt(t *testing.T) {
	host := setupCommandTestHost(t)
	host.previewRoot = "/home/alice/site"
	host.previewFiles = 17
	confirmations := 0
	confirm := func(_ context.Context, confirmation PublicConfirmation) (bool, error) {
		confirmations++
		if confirmation.Host.Alias != "pc" || confirmation.Service.Target != "/home/alice/site" || confirmation.FileCount != 17 || confirmation.URL != "https://blog.shaulavo.dev/blog" {
			t.Fatalf("confirmation = %#v", confirmation)
		}
		return true, nil
	}
	stdout, _, err := executeCommand(t, Dependencies{DialControl: host.dial, ConfirmPublic: confirm},
		"serve", "pc", "./site", "--at", "/blog", "--public", "blog.shaulavo.dev")
	if err != nil {
		t.Fatal(err)
	}
	if confirmations != 1 || !strings.Contains(stdout, "https://blog.shaulavo.dev/blog") {
		t.Fatalf("confirmations = %d, output %q", confirmations, stdout)
	}
	_, _, err = executeCommand(t, Dependencies{
		DialControl: host.dial,
		ConfirmPublic: func(context.Context, PublicConfirmation) (bool, error) {
			t.Fatal("--yes called confirmation adapter")
			return false, nil
		},
	}, "serve", "pc", "./site", "--at", "/blog", "--public", "blog.shaulavo.dev", "--yes")
	if err != nil {
		t.Fatal(err)
	}
}

func TestServeCommandRejectsMeaninglessFlagsBeforeDial(t *testing.T) {
	host := setupCommandTestHost(t)
	for _, args := range [][]string{
		{"serve", "pc", "3000", "--at", "/files", "--files"},
		{"serve", "pc", "./site", "--at", "/site", "--allow-credentials"},
		{"serve", "pc", "./site", "--at", "/site", "--wake-on-request"},
		{"serve", "pc", "./site", "--at", "/site", "--yes"},
	} {
		if _, _, err := executeCommand(t, Dependencies{DialControl: host.dial}, args...); err == nil {
			t.Fatalf("arguments %v were accepted", args)
		}
	}
	if events := host.recorded(); len(events) != 0 {
		t.Fatalf("invalid forms dialed host: %v", events)
	}
}

func TestServiceTableCellCannotInjectTerminalControls(t *testing.T) {
	got := safeTableCell("/srv/site\tFAKE\nROW\x1b[31m\u202ereversed")
	if strings.ContainsAny(got, "\t\n\r\x1b") || strings.ContainsRune(got, '\u202e') || !strings.Contains(got, `\tFAKE\nROW\x1b`) || !strings.Contains(got, `\u202e`) {
		t.Fatalf("safe table cell = %q", got)
	}
	if long := SafeTerminalText(strings.Repeat("x", 10_000)); len(long) > maximumTerminalTextBytes || !strings.HasSuffix(long, "...") {
		t.Fatalf("bounded terminal text has %d bytes: %q", len(long), long)
	}
}

func TestProtocolSessionTableEscapesAndBoundsRemoteCommand(t *testing.T) {
	var output bytes.Buffer
	malicious := "ATTACKER\tFAKE\nROW\x1b[31m\u202e" + strings.Repeat("x", 10_000)
	err := writeProtocolSessions(&output, commandTestTime, []HostSessions{{
		Host: HostRecord{Alias: "pc"}, Sessions: []protocol.SessionInfo{{
			ID: "7K3D", HostID: "host-id", Command: []string{malicious}, State: "running", CreatedAt: commandTestTime,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if strings.ContainsAny(got, "\x1b\r") || strings.ContainsRune(got, '\u202e') || strings.Count(got, "\n") != 2 || len(got) > 1500 {
		t.Fatalf("unsafe session table output = %q (%d bytes)", got, len(got))
	}
}

func TestSessionListDiagnosticsCannotInjectTerminalControls(t *testing.T) {
	setupCommandTestHost(t)
	cause := errors.New("ATTACKER\r\n\u202e" + strings.Repeat("x", 10_000))
	_, stderr, err := executeCommand(t, Dependencies{DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
		return nil, cause
	}}, "ls", "--timeout", "100ms")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(stderr, "\r\x1b") || strings.ContainsRune(stderr, '\u202e') || strings.Count(stderr, "\n") != 1 || len(stderr) > maximumRemoteErrorBytes+200 {
		t.Fatalf("unsafe list diagnostic = %q (%d bytes)", stderr, len(stderr))
	}
}

func TestTerminalPublicConfirmationCancelsWithoutLeakingARead(t *testing.T) {
	t.Setenv("TERM", "dumb")
	master, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()   //nolint:errcheck // test resource cleanup
	defer terminal.Close() //nolint:errcheck // test resource cleanup
	confirmation := PublicConfirmation{Host: HostRecord{Alias: "pc"}, Service: protocol.ServiceInfo{Kind: "proxy", Target: "3000"}, URL: "https://app.shaulavo.dev/api"}
	for iteration := 0; iteration < 8; iteration++ {
		output := newPromptTestWriter()
		confirm := terminalPublicConfirmation(terminal, output)
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, confirmErr := confirm(ctx, confirmation)
			result <- confirmErr
		}()
		output.wait(t)
		cancel()
		select {
		case confirmErr := <-result:
			if !errors.Is(confirmErr, context.Canceled) {
				t.Fatalf("cancelled confirmation %d error = %v", iteration, confirmErr)
			}
		case <-time.After(time.Second):
			t.Fatalf("cancelled confirmation %d remained blocked", iteration)
		}
	}

	output := newPromptTestWriter()
	result := make(chan struct {
		confirmed bool
		err       error
	}, 1)
	go func() {
		confirmed, confirmErr := terminalPublicConfirmation(terminal, output)(context.Background(), confirmation)
		result <- struct {
			confirmed bool
			err       error
		}{confirmed: confirmed, err: confirmErr}
	}()
	output.wait(t)
	if _, err := master.Write([]byte("yes\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || !got.confirmed {
			t.Fatalf("confirmation after cancellations = %v, %v", got.confirmed, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("confirmation read was consumed by a leaked prompt")
	}
}

func TestServeListPrintsActualPrivateAndPublicURLs(t *testing.T) {
	host := setupCommandTestHost(t)
	host.services = []protocol.ServiceInfo{
		{Name: "api", Kind: "proxy", Target: "3000", Healthy: true},
		{Name: "blog", Kind: "static", Target: "/home/alice/site", PublicName: "blog.shaulavo.dev", Healthy: true},
	}
	host.edgeRoutes = []protocol.EdgeRouteInfo{{
		PublicName: "blog.shaulavo.dev", ServiceName: "blog", DisplayAlias: "pc", LastSeenAt: commandTestTime, Online: true,
	}}
	for _, listCommand := range []string{"ls", "list"} {
		t.Run(listCommand, func(t *testing.T) {
			stdout, _, err := executeCommand(t, Dependencies{DialControl: host.dial, Now: func() time.Time { return commandTestTime }}, "serve", listCommand)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"ROUTE", "HOST", "KIND", "TARGET", "SCOPE", "HEALTH", "URL",
				"https://pc.mesh.shaulavo.dev/api", "https://blog.shaulavo.dev/blog", "tailnet", "public", "healthy",
			} {
				if !strings.Contains(stdout, want) {
					t.Fatalf("serve %s output %q does not contain %q", listCommand, stdout, want)
				}
			}
		})
	}
}

func TestUnserveRefusesAmbiguityAndHostFlagSelectsLiveOwner(t *testing.T) {
	pc := setupCommandTestHost(t)
	pc.services = []protocol.ServiceInfo{{Name: "blog", Kind: "proxy", Target: "3000", Healthy: true}}
	pi := &commandTestHost{host: HostRecord{
		Alias: "pi", ID: "pi-id", MeshIdentity: "pi-key", TailscaleName: "pi.example.ts.net",
		Addresses: []string{"100.64.0.3"}, Endpoint: "ws://100.64.0.3:7777/mesh",
	}, services: []protocol.ServiceInfo{{Name: "blog", Kind: "proxy", Target: "4000", Healthy: true}}}
	if err := SaveHost(pi.host); err != nil {
		t.Fatal(err)
	}
	dial := func(ctx context.Context, host HostRecord) (transport.Conn, error) {
		if host.ID == pc.host.ID {
			return pc.dial(ctx, host)
		}
		return pi.dial(ctx, host)
	}
	if _, _, err := executeCommand(t, Dependencies{DialControl: dial}, "unserve", "/blog"); err == nil || !strings.Contains(err.Error(), "multiple hosts") {
		t.Fatalf("ambiguous unserve error = %v", err)
	}
	stdout, _, err := executeCommand(t, Dependencies{DialControl: dial}, "unserve", "/blog", "--host", "pc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "unserved /blog on pc") || len(pc.services) != 0 || len(pi.services) != 1 {
		t.Fatalf("unserve output %q, pc %#v, pi %#v", stdout, pc.services, pi.services)
	}
}

func TestUnserveRequiresHostWhenAnotherOwnerCannotBeQueried(t *testing.T) {
	pc := setupCommandTestHost(t)
	pc.services = []protocol.ServiceInfo{{Name: "blog", Kind: "proxy", Target: "3000", Healthy: true}}
	pi := HostRecord{
		Alias: "pi", ID: "pi-id", MeshIdentity: "pi-key", TailscaleName: "pi.example.ts.net",
		Addresses: []string{"100.64.0.3"}, Endpoint: "ws://100.64.0.3:7777/mesh",
	}
	if err := SaveHost(pi); err != nil {
		t.Fatal(err)
	}
	dial := func(ctx context.Context, host HostRecord) (transport.Conn, error) {
		if host.ID == pc.host.ID {
			return pc.dial(ctx, host)
		}
		return nil, errors.New("offline")
	}
	if _, _, err := executeCommand(t, Dependencies{DialControl: dial}, "unserve", "/blog", "--timeout", "30ms"); err == nil || !strings.Contains(err.Error(), "could not prove") {
		t.Fatalf("incomplete ownership error = %v", err)
	}
	if len(pc.services) != 1 {
		t.Fatalf("route changed after incomplete discovery: %#v", pc.services)
	}
	if _, _, err := executeCommand(t, Dependencies{DialControl: dial}, "unserve", "/blog", "--host", "pc"); err != nil {
		t.Fatal(err)
	}
}

func TestUnserveDoesNotClaimExplicitOfflineHostLacksRoute(t *testing.T) {
	setupCommandTestHost(t)
	_, _, err := executeCommand(t, Dependencies{DialControl: func(context.Context, HostRecord) (transport.Conn, error) {
		return nil, errors.New("offline")
	}}, "unserve", "/blog", "--host", "pc", "--timeout", "20ms")
	if err == nil || !strings.Contains(err.Error(), "host pc is unavailable") {
		t.Fatalf("explicit offline host error = %v", err)
	}
}

func TestUnserveExplicitAbsentRetryPreservesAuthenticatedPrivateName(t *testing.T) {
	host := setupCommandTestHost(t)
	host.services = []protocol.ServiceInfo{{Name: "api", Kind: "proxy", Target: "3000", Healthy: true}}

	if _, _, err := executeCommand(t, Dependencies{DialControl: host.dial}, "unserve", "/missing", "--host", "pc"); err != nil {
		t.Fatal(err)
	}
	cache, err := OpenCatalogCache(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close() //nolint:errcheck // test cleanup
	rows, err := cache.LoadAllServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := rows[host.host.ID]
	if len(got) != 1 || got[0].Service.Name != "api" || got[0].PrivateName != "pc.mesh.shaulavo.dev" {
		t.Fatalf("cached services after absent retry = %#v", got)
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
	if !strings.Contains(stdout, "pc was already up to date (host-id)") {
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

func TestAddPassesTailscaleProvisioningFlagsToBootstrap(t *testing.T) {
	t.Setenv("MESH_CONFIG_DIR", t.TempDir())
	called := false
	bootstrap := func(_ context.Context, request AddRequest) (BootstrapResult, error) {
		called = true
		if request.Target != "alice@pi" || request.Alias != "garden" || request.TailscaleAuthKeyFile != "./tailnet.key" || !request.Yes {
			t.Fatalf("add request = %#v", request)
		}
		return BootstrapResult{Host: HostRecord{
			ID: "host-id", MeshIdentity: "mesh-identity", TailscaleName: "pi.example.ts.net",
			Addresses: []string{"100.64.0.8"}, Endpoint: "ws://100.64.0.8:7337/mesh",
		}}, nil
	}
	_, _, err := executeCommand(t, Dependencies{Bootstrap: bootstrap},
		"add", "alice@pi", "--alias", "garden", "--tailscale-auth-key-file", "./tailnet.key", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("bootstrap was not called")
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

func TestPickerReturnsBeforeRawAttachStarts(t *testing.T) {
	host := setupCommandTestHost(t)
	pickerReturned := false
	host.attachStart = func() {
		if !pickerReturned {
			t.Fatal("raw attach started before the picker returned")
		}
	}
	_, _, err := executeCommand(t, Dependencies{
		DialHost: host.dial,
		Picker: func(context.Context, PickerInput) (PickerSelection, error) {
			pickerReturned = true
			return PickerSelection{HostAlias: "pc", SessionID: "7K3D"}, nil
		},
	}, "--raw")
	if err != nil {
		t.Fatal(err)
	}
	if !pickerReturned || !slices.Contains(host.recorded(), protocol.TypeAttach) {
		t.Fatalf("picker returned = %t, events = %v", pickerReturned, host.recorded())
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
	defer stdin.Close()  //nolint:errcheck // test resource cleanup
	defer stdout.Close() //nolint:errcheck // test resource cleanup
	defer stderr.Close() //nolint:errcheck // test resource cleanup
	dependencies.Stdin = stdin
	dependencies.Stdout = stdout
	dependencies.Stderr = stderr
	command := NewCommand(dependencies)
	command.SetArgs(args)
	err = command.ExecuteContext(context.Background())
	return readCommandFile(t, stdout), readCommandFile(t, stderr), err
}

type promptTestWriter struct {
	mu    sync.Mutex
	text  bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func newPromptTestWriter() *promptTestWriter { return &promptTestWriter{ready: make(chan struct{})} }

func (w *promptTestWriter) Write(contents []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written, err := w.text.Write(contents)
	if strings.Contains(w.text.String(), "Continue? [y/N]") {
		w.once.Do(func() { close(w.ready) })
	}
	return written, err
}

func (w *promptTestWriter) wait(t *testing.T) {
	t.Helper()
	select {
	case <-w.ready:
	case <-time.After(time.Second):
		t.Fatal("confirmation prompt was not written")
	}
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

// writeLocalSessionDir plants a session on this machine the way a worker does,
// so the CLI's local lookup has something real to find.
func writeLocalSessionDir(t *testing.T, id string, state string) {
	t.Helper()
	root, err := paths.SessionsDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := worker.Meta{
		ID:        id,
		PID:       4321,
		Command:   []string{"sh", "-c", "sleep 300"},
		Cwd:       "/work",
		State:     state,
		CreatedAt: commandTestTime,
	}
	if state == worker.StateExited {
		exited := commandTestTime.Add(time.Second)
		code := 0
		meta.ExitedAt = &exited
		meta.ExitCode = &code
	}
	if err := worker.WriteMeta(dir, meta); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker.log"), []byte("local output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// This host is never in its own address book, so a lookup gated on an empty
// address book made every local session vanish from ls, logs, kill and sig the
// moment the first remote host was adopted — while the worker kept running and
// there was no other way to reach it.
func TestLocalSessionsStayReachableAfterAdoptingAHost(t *testing.T) {
	setupCommandTestHost(t)
	writeLocalSessionDir(t, "L0CL", worker.StateExited)

	stdout, _, err := executeCommand(t, Dependencies{
		DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
			return nil, errors.New("host is offline")
		},
		Now: func() time.Time { return commandTestTime.Add(time.Minute) },
	}, "ls", "--timeout", "20ms")
	if err != nil {
		t.Fatalf("ls returned an error: %v", err)
	}
	if !strings.Contains(stdout, "L0CL") {
		t.Fatalf("ls output = %q, want the local session listed alongside adopted hosts", stdout)
	}

	// The same session must remain addressable by ID for the commands that act
	// on one: logs is the read-only one, so it is the safe probe.
	if _, _, err := executeCommand(t, Dependencies{
		DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
			return nil, errors.New("host is offline")
		},
		Now: func() time.Time { return commandTestTime.Add(time.Minute) },
	}, "logs", "L0CL"); err != nil {
		t.Fatalf("logs on a local session after adoption: %v", err)
	}
}

// Alive is a 500ms socket dial. On a loaded machine it can miss a perfectly
// healthy session, and kill then refused with "already interrupted" — exactly
// when kill matters most. Only a state the worker actually recorded may refuse
// the operation.
func TestKillDoesNotRefuseOnAMissedLivenessProbe(t *testing.T) {
	setupCommandTestHost(t)
	// A session whose meta says running but whose socket does not answer: the
	// same shape a loaded machine produces for a live session.
	writeLocalSessionDir(t, "PR0B", worker.StateRunning)

	_, _, err := executeCommand(t, Dependencies{
		DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
			return nil, errors.New("host is offline")
		},
		Now: func() time.Time { return commandTestTime },
	}, "kill", "PR0B")
	if err == nil {
		t.Fatal("kill reported success against an unreachable worker")
	}
	// It must have attempted the kill and reported the session's real state,
	// not refused up front on the probe.
	if !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("kill error = %v, want the session reported as interrupted", err)
	}

	// A session the worker recorded as exited is still refused up front, since
	// that record is definitive rather than inferred.
	writeLocalSessionDir(t, "3X1T", worker.StateExited)
	_, _, err = executeCommand(t, Dependencies{
		DialHost: func(context.Context, HostRecord) (transport.Conn, error) {
			return nil, errors.New("host is offline")
		},
		Now: func() time.Time { return commandTestTime },
	}, "kill", "3X1T")
	if err == nil || !strings.Contains(err.Error(), "already exited") {
		t.Fatalf("kill on an exited session = %v, want refusal", err)
	}
}
