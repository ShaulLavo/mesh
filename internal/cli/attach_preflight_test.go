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
						return PickerSelection{HostAlias: "pc", SessionID: "7K3D", Relaunch: true, RecoveryAction: recovery.ActionCommand}, nil
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
		t.Run(stage, func(t *testing.T) {
			host := setupCommandTestHost(t)
			var attachment *cancelCommandAttachConn
			dial := func(ctx context.Context, record HostRecord) (transport.Conn, error) {
				conn, err := host.dial(ctx, record)
				if err != nil {
					return nil, err
				}
				attachment = &cancelCommandAttachConn{
					commandTestConn: conn.(*commandTestConn), stage: stage,
					entered: make(chan struct{}), closed: make(chan struct{}),
				}
				return attachment, nil
			}
			input, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = input.Close(); _ = writer.Close() })
			output, err := os.CreateTemp(t.TempDir(), "output")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = output.Close() })
			command := NewCommand(Dependencies{
				DialHost: dial, DialControl: dial, Stdin: input, Stdout: output, Stderr: output,
				Containment: func(context.Context) []protocol.SessionIdentity { return nil },
			})
			command.SetArgs([]string{"pc", "-r", "--raw"})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			baselineInput := goroutinesWithStack("internal/cli.relayInput")
			baselineResize := goroutinesWithStack("internal/cli.relayTerminalResizes")
			baselineLocalResize := goroutinesWithStack("internal/cli.relayResizes")
			done := make(chan error, 1)
			ready := make(chan *cancelCommandAttachConn, 1)
			host.attachStart = func() { ready <- attachment }
			go func() { done <- command.ExecuteContext(ctx) }()
			select {
			case attachment = <-ready:
			case <-time.After(time.Second):
				t.Fatal("production attach did not start")
			}
			<-attachment.entered
			if stage == "after acknowledgement" {
				deadline := time.Now().Add(time.Second)
				for goroutinesWithStack("internal/cli.relayInput") <= baselineInput {
					if time.Now().After(deadline) {
						_ = attachment.Close()
						<-done
						t.Fatal("production input relay did not start")
					}
					time.Sleep(time.Millisecond)
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled production attachment = %v, want context.Canceled", err)
				}
			case <-time.After(time.Second):
				_ = attachment.Close()
				<-done
				t.Fatal("production attachment ignored command cancellation; forced connection closure")
			}
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
				if got := goroutinesWithStack(relay.name); got != relay.want {
					t.Errorf("%s goroutines = %d, want %d", relay.name, got, relay.want)
				}
			}
			if host.eventCount(protocol.TypeSignal)+host.eventCount(protocol.TypeKill) != 0 {
				t.Fatal("command cancellation signalled the worker")
			}
		})
	}
}

type cancelCommandAttachConn struct {
	*commandTestConn
	stage   string
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *cancelCommandAttachConn) ReadFrame() (protocol.Frame, error) {
	select {
	case <-c.closed:
		return protocol.Frame{}, net.ErrClosed
	case frame := <-c.responses:
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
	c.host.record(request.Type)
	c.host.attachStart()
	close(c.entered)
	if c.stage == "initial write" {
		<-c.closed
		return net.ErrClosed
	}
	if c.stage == "after acknowledgement" {
		c.responses <- mustCommandControlFrame(protocol.Control{Type: protocol.TypeAttached, SessionID: request.SessionID})
	}
	return nil
}

func (c *cancelCommandAttachConn) Close() error {
	c.once.Do(func() { close(c.closed) })
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
	state, err := os.MkdirTemp("", "mesh-wave-u-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
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
