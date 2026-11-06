package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	meshdaemon "github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/recovery"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func TestInvalidAttachFlagsDoNotCreateSession(t *testing.T) {
	flags := []struct {
		name string
		args []string
	}{
		{name: "detach", args: []string{"--detach-key", "not-a-key"}},
		{name: "leave", args: []string{"--leave-key", "not-a-key"}},
		{name: "collision", args: []string{"--detach-key", "ctrl+a", "--leave-key", "ctrl+a"}},
	}
	for _, entry := range []string{"remote", "local", "recovery", "picker restart"} {
		for _, flag := range flags {
			t.Run(entry+"/"+flag.name, func(t *testing.T) {
				host := setupCommandTestHost(t)
				var localCreates atomic.Int32
				startAttachTestDaemon(t, func(request protocol.Control) protocol.Control {
					if request.Type == protocol.TypeCreate {
						localCreates.Add(1)
					}
					return protocol.Control{Type: protocol.TypeError, Message: "unexpected lifecycle request"}
				})
				deps := Dependencies{DialHost: host.dial, DialControl: host.dial}
				args := []string{"pc"}
				switch entry {
				case "local":
					args = []string{"local", "--daemon"}
				case "recovery":
					host.sessionState, host.recoverTo = worker.StateInterrupted, "91AZ"
					args = []string{"recover", "7K3D"}
				case "picker restart":
					host.sessionState = worker.StateInterrupted
					deps.Picker = func(context.Context, PickerInput) (PickerSelection, error) {
						return PickerSelection{HostID: "khI9qfAZ1eqQXe4C2JhMIfS8lwSL_GC5Aef-MsKEYZE", SessionID: "7K3D", Relaunch: true, RecoveryAction: recovery.ActionCommand}, nil
					}
					args = nil
				}
				_, _, err := executeCommand(t, deps, append(args, flag.args...)...)
				if err == nil {
					t.Fatal("invalid attach flags succeeded")
				}
				if got := host.eventCount(protocol.TypeCreate) + int(localCreates.Load()); got != 0 {
					t.Errorf("invalid attach flags created %d sessions", got)
				}
				if got := host.eventCount(protocol.TypeRecover); got != 0 {
					t.Errorf("invalid attach flags sent %d recovery requests", got)
				}
				if !strings.Contains(err.Error(), "key") {
					t.Errorf("error = %v, want attach key validation", err)
				}
			})
		}
	}
}

func TestProductionAttachmentHonorsCommandCancellation(t *testing.T) {
	for _, stage := range []string{"before handshake", "initial write", "after acknowledgement"} {
		t.Run(stage, func(t *testing.T) { testProductionAttachmentCancellation(t, stage) })
	}
}

func testProductionAttachmentCancellation(t *testing.T, stage string) {
	t.Helper()
	trace := newCancellationPhaseTrace(t)
	host := setupCommandTestHost(t)
	trace.phase("host/catalog ready")
	t.Cleanup(func() { trace.phase("host/catalog cleanup") })
	var attachment *cancelCommandAttachConn
	dial := func(ctx context.Context, record HostRecord) (transport.Conn, error) {
		trace.event("host dial started")
		conn, err := host.dial(ctx, record)
		if err != nil {
			return nil, err
		}
		attachment = &cancelCommandAttachConn{
			commandTestConn: conn.(*commandTestConn), stage: stage, trace: trace,
			entered: make(chan struct{}), closed: make(chan struct{}),
		}
		trace.event("host dial returned")
		return attachment, nil
	}
	trace.phase("input pipe setup")
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		trace.phase("close input")
		_ = input.Close()
		trace.phase("close input writer")
		_ = writer.Close()
		trace.phase("input closed")
	})
	trace.phase("output scratch setup")
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		trace.phase("close output")
		_ = output.Close()
		trace.phase("output scratch cleanup")
	})
	trace.phase("command setup")
	command := NewCommand(Dependencies{
		DialHost: dial, DialControl: dial, Stdin: input, Stdout: output, Stderr: output,
		Containment: func(context.Context) []protocol.SessionIdentity { return nil },
	})
	command.SetArgs([]string{"pc", "-r", "--raw"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	trace.phase("count baseline input relays")
	baselineInput := goroutinesWithStack("internal/cli.relayInput")
	trace.phase("count baseline terminal resize relays")
	baselineResize := goroutinesWithStack("internal/cli.relayTerminalResizes")
	trace.phase("count baseline local resize relays")
	baselineLocalResize := goroutinesWithStack("internal/cli.relayResizes")
	done := make(chan error, 1)
	ready := make(chan *cancelCommandAttachConn, 1)
	host.attachStart = func() { ready <- attachment }
	trace.phase("wait handshake")
	go func() {
		trace.event("ExecuteContext started")
		err := command.ExecuteContext(ctx)
		trace.event("ExecuteContext returned")
		done <- err
	}()
	select {
	case attachment = <-ready:
	case <-time.After(time.Second):
		t.Fatal("production attach did not start")
	}
	trace.phase("wait attachment entered")
	<-attachment.entered
	trace.phase("attachment entered")
	if stage == "after acknowledgement" {
		trace.phase("wait input relay after acknowledgement")
		waitForCancellationInputRelay(t, attachment, done, baselineInput)
	}
	trace.phase("cancel command")
	cancel()
	trace.event("cancel returned; context error: " + ctx.Err().Error())
	trace.phase("wait ExecuteContext return")
	select {
	case err := <-done:
		trace.phase("command joined")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled production attachment = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		trace.phase("forced connection closure after cancellation deadline")
		_ = attachment.Close()
		trace.phase("join command after forced connection closure")
		<-done
		t.Fatal("production attachment ignored command cancellation; forced connection closure")
	}
	trace.phase("check connection closed")
	select {
	case <-attachment.closed:
	default:
		t.Fatal("command cancellation left the client connection open")
	}
	for _, relay := range []struct {
		name string
		want int
	}{
		{"internal/cli.relayInput", baselineInput},
		{"internal/cli.relayTerminalResizes", baselineResize},
		{"internal/cli.relayResizes", baselineLocalResize},
	} {
		trace.phase("check joined relay " + relay.name)
		if got := goroutinesWithStack(relay.name); got != relay.want {
			t.Errorf("%s goroutines = %d, want %d", relay.name, got, relay.want)
		}
	}
	trace.phase("check worker not signalled")
	if host.eventCount(protocol.TypeSignal)+host.eventCount(protocol.TypeKill) != 0 {
		t.Fatal("command cancellation signalled the worker")
	}
	trace.phase("test body complete")
}

func waitForCancellationInputRelay(t *testing.T, attachment *cancelCommandAttachConn, done <-chan error, baselineInput int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for goroutinesWithStack("internal/cli.relayInput") <= baselineInput {
		if time.Now().After(deadline) {
			attachment.trace.phase("forced connection closure after relay deadline")
			_ = attachment.Close()
			attachment.trace.phase("join command after relay deadline")
			<-done
			t.Fatal("production input relay did not start")
		}
		time.Sleep(time.Millisecond)
	}
}

type cancelCommandAttachConn struct {
	*commandTestConn
	stage   string
	trace   *cancellationPhaseTrace
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *cancelCommandAttachConn) ReadFrame() (protocol.Frame, error) {
	select {
	case <-c.closed:
		c.trace.event("attachment read observed connection closed")
		return protocol.Frame{}, net.ErrClosed
	case frame := <-c.responses:
		c.trace.event("attachment read received response")
		return frame, nil
	}
}

func (c *cancelCommandAttachConn) WriteFrame(frame protocol.Frame) error {
	if frame.Kind != protocol.KindControl {
		return nil
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("decode test attachment request: %w", err)
	}
	if request.Type != protocol.TypeAttach {
		return c.commandTestConn.WriteFrame(frame)
	}
	c.trace.event("attachment write entered")
	c.host.record(request.Type)
	c.host.attachStart()
	close(c.entered)
	if c.stage == "initial write" {
		c.trace.event("initial write waiting for connection close")
		<-c.closed
		c.trace.event("initial write observed connection closed")
		return net.ErrClosed
	}
	if c.stage == "after acknowledgement" {
		c.trace.event("send acknowledgement")
		c.responses <- mustCommandControlFrame(protocol.Control{Type: protocol.TypeAttached, SessionID: request.SessionID})
		c.trace.event("acknowledgement sent")
	}
	return nil
}

func (c *cancelCommandAttachConn) Close() error {
	c.once.Do(func() {
		c.trace.event("connection close started")
		close(c.closed)
		c.trace.event("connection closed")
	})
	return nil
}

func TestStartWindowSessionRetriesAreBoundedWithoutDuplicateCreation(t *testing.T) {
	setupCommandTestHost(t)
	var creates, attaches atomic.Int32
	startAttachTestDaemon(t, func(request protocol.Control) protocol.Control {
		switch request.Type {
		case protocol.TypeCreate:
			creates.Add(1)
			return protocol.Control{Type: protocol.TypeCreated, SessionID: "7K3D"}
		case protocol.TypeAttachDetached:
			if attaches.Add(1) > 3 {
				return protocol.Control{Type: protocol.TypeError, Message: "test stopped an unbounded window retry"}
			}
			return protocol.Control{Type: protocol.TypeError, Reason: protocol.ReasonAttached, Message: "another client claimed this session"}
		default:
			return protocol.Control{Type: protocol.TypeError, Message: "unexpected request"}
		}
	})
	app := &application{dependencies: Dependencies{
		Containment: func(context.Context) []protocol.SessionIdentity { return nil },
		Terminal:    func() (TerminalIdentity, bool) { return TerminalIdentity{}, false },
	}}
	cmd := &cobra.Command{}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	cmd.SetContext(ctx)
	err := app.startWindowSession(cmd, "", true)
	if !errors.Is(err, ErrSessionAttached) || !strings.Contains(err.Error(), "start window session") {
		t.Errorf("bounded window error = %v, want wrapped ErrSessionAttached", err)
	}
	if got := creates.Load(); got != 1 {
		t.Errorf("window created %d sessions, want exactly one", got)
	}
	if got := attaches.Load(); got != 3 {
		t.Errorf("window made %d attach attempts, want three", got)
	}
}

func TestHostResumeUsesMostRecentlyActiveSession(t *testing.T) {
	host := setupCommandTestHost(t)
	attached := commandTestTime.Add(2 * time.Hour)
	host.listRows = func() []protocol.SessionInfo {
		return []protocol.SessionInfo{
			{ID: "91AZ", HostID: host.host.ID, State: worker.StateDetached, Command: []string{"bash"}, Cwd: "/work", CreatedAt: commandTestTime.Add(time.Hour)},
			{ID: "7K3D", HostID: host.host.ID, State: worker.StateDetached, Command: []string{"bash"}, Cwd: "/work", CreatedAt: commandTestTime, LastAttachedAt: &attached},
		}
	}
	if _, _, err := executeCommand(t, Dependencies{DialHost: host.dial, DialControl: host.dial}, "pc", "-r", "--raw"); err != nil {
		t.Fatal(err)
	}
	if got := host.attached().SessionID; got != "7K3D" {
		t.Fatalf("remote resume attached %s, want most recently active 7K3D", got)
	}
}

func startAttachTestDaemon(t *testing.T, handle func(protocol.Control) protocol.Control) {
	t.Helper()
	state := compactSocketTempDir(t)
	t.Setenv("MESH_STATE_DIR", state)
	listener, err := net.Listen("unix", meshdaemon.SocketPath(state))
	if err != nil {
		t.Fatal(err)
	}
	var servers sync.WaitGroup
	servers.Go(func() {
		for {
			stream, err := listener.Accept()
			if err != nil {
				return
			}
			servers.Go(func() {
				defer stream.Close() //nolint:errcheck // fixture connection cleanup
				frame, err := protocol.NewReader(stream).ReadFrame()
				if err != nil {
					t.Errorf("daemon read: %v", err)
					return
				}
				request, err := protocol.DecodeControl(frame.Payload)
				if err != nil {
					t.Errorf("daemon decode: %v", err)
					return
				}
				response := handle(request)
				response.RequestID = request.RequestID
				if response.SessionID == "" {
					response.SessionID = request.SessionID
				}
				if err := protocol.NewWriter(stream).WriteControlMsg(response); err != nil {
					t.Errorf("daemon response: %v", err)
				}
			})
		}
	})
	t.Cleanup(func() { _ = listener.Close(); servers.Wait() })
}
