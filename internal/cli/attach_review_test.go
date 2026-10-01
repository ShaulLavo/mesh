//go:build !windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/testenv"
	"github.com/shaul/mesh/internal/transport"
)

func TestAttachPTYControlCCancelsEstablishment(t *testing.T) {
	for _, stage := range []string{"initial write", "handshake"} {
		t.Run(stage, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestAttachPTYProcess$") //nolint:gosec // isolated child of this test binary
			command.Env = append(testenv.ForProcess(t.TempDir()), "MESH_ATTACH_PTY_TEST="+stage)
			master, err := pty.Start(command)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = master.Close(); _ = command.Process.Kill() })
			chunks := make(chan []byte, 8)
			go readAgentTestTerminal(master, chunks)
			exited := make(chan error, 1)
			go func() { exited <- command.Wait() }()
			var output bytes.Buffer
			awaitAgentTerminalMarker(t, ctx, chunks, &output, "ESTABLISHING")
			if _, err := master.Write([]byte{3}); err != nil {
				t.Fatal(err)
			}
			// Observe restoration itself, not process exit: race binaries wait
			// an extra second on exit even after Attach has already returned.
			returned, stop := context.WithTimeout(ctx, time.Second)
			for !strings.Contains(output.String(), "CANCELLED_RESTORED") {
				chunk, err := nextAgentTerminalChunk(returned, chunks)
				if err != nil {
					_ = command.Process.Signal(syscall.SIGTERM)
					t.Errorf("typed Ctrl-C did not cancel %s; external SIGTERM was required", stage)
					break
				}
				output.Write(chunk)
			}
			stop()
			if err := <-exited; err != nil {
				t.Fatalf("PTY child failed: %v; output=%q", err, output.String())
			}
		})
	}
}

func TestAttachPTYProcess(t *testing.T) {
	stage := os.Getenv("MESH_ATTACH_PTY_TEST")
	if stage == "" {
		return
	}
	before, err := term.GetState(os.Stdin.Fd())
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(t.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	client, server := net.Pipe()
	defer server.Close() //nolint:errcheck // child fixture cleanup
	conn, err := transport.NewStreamConn(client)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		var err error
		if stage == "initial write" {
			_, err = io.ReadFull(server, make([]byte, 5))
		} else {
			_, err = protocol.NewReader(server).ReadFrame()
		}
		_, _ = fmt.Fprintln(os.Stderr, "ESTABLISHING")
		ready <- err
	}()
	_, err = Attach(ctx, AttachOptions{SessionID: "7K3D", Conn: conn, In: os.Stdin, Out: os.Stdout})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PTY attach error = %v", err)
	}
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	after, err := term.GetState(os.Stdin.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("attachment did not restore termios")
	}
	_, _ = fmt.Fprintln(os.Stderr, "CANCELLED_RESTORED")
}

func TestCancelledAttachmentBindingDependsOnAcknowledgement(t *testing.T) {
	for _, previous := range []string{"", "91AZ"} {
		for _, acknowledged := range []bool{false, true} {
			t.Run(fmt.Sprintf("previous=%s/ack=%t", previous, acknowledged), func(t *testing.T) {
				host := setupCommandTestHost(t)
				const key = "cancel-tab"
				if previous != "" {
					if err := saveTerminalBinding(key, TerminalBinding{SessionID: previous, BoundAt: commandTestTime}); err != nil {
						t.Fatal(err)
					}
				}
				ready := make(chan *bindingCancelConn, 1)
				dial := func(ctx context.Context, record HostRecord) (transport.Conn, error) {
					conn, err := host.dial(ctx, record)
					if err != nil {
						return nil, err
					}
					stage := "before handshake"
					if acknowledged {
						stage = "after acknowledgement"
					}
					attachment := &bindingCancelConn{cancelCommandAttachConn: &cancelCommandAttachConn{
						commandTestConn: conn.(*commandTestConn), stage: stage,
						entered: make(chan struct{}), closed: make(chan struct{}),
					}, processed: make(chan struct{})}
					host.attachStart = func() { ready <- attachment }
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
				command := NewCommand(Dependencies{DialHost: dial, Stdin: input, Stdout: output, Stderr: output,
					Terminal: fakeTerminal(key), Containment: func(context.Context) []protocol.SessionIdentity { return nil }})
				command.SetArgs([]string{"pc", "-r", "--raw"})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- command.ExecuteContext(ctx) }()
				var attachment *bindingCancelConn
				select {
				case attachment = <-ready:
				case <-time.After(time.Second):
					t.Fatal("attachment did not start")
				}
				if acknowledged {
					select {
					case <-attachment.processed:
					case <-time.After(time.Second):
						t.Fatal("acknowledgement was not processed")
					}
				}
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancelled command = %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("cancelled command did not return")
				}
				want := previous
				if acknowledged {
					want = "7K3D"
				}
				binding, found := loadTerminalBinding(key)
				if found != (want != "") || binding.SessionID != want {
					t.Fatalf("binding after cancellation = %q, found=%t; want %q", binding.SessionID, found, want)
				}
				if host.eventCount(protocol.TypeSignal)+host.eventCount(protocol.TypeKill)+host.eventCount(protocol.TypeDetach) != 0 {
					t.Fatal("cancellation sent worker lifecycle control")
				}
			})
		}
	}
}

type bindingCancelConn struct {
	*cancelCommandAttachConn
	processed chan struct{}
	ack       bool
	once      sync.Once
}

func (c *bindingCancelConn) ReadFrame() (protocol.Frame, error) {
	if c.ack {
		c.once.Do(func() { close(c.processed) })
	}
	frame, err := c.cancelCommandAttachConn.ReadFrame()
	if err == nil && frame.Kind == protocol.KindControl {
		message, _ := protocol.DecodeControl(frame.Payload)
		if message.Type == protocol.TypeAttached {
			c.ack = true
		}
	}
	return frame, err
}

func TestAttachCancellationUnblocksFullOutputAndRestoresPTY(t *testing.T) {
	master, input, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = input.Close() })
	before, err := term.GetState(input.Fd())
	if err != nil {
		t.Fatal(err)
	}
	reader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = output.Close() })
	conn := &outputCancelConn{frames: make(chan protocol.Frame, 2), closed: make(chan struct{})}
	conn.frames <- mustCommandControlFrame(protocol.Control{Type: protocol.TypeAttached, SessionID: "7K3D"})
	conn.frames <- protocol.Frame{Kind: protocol.KindData, Payload: bytes.Repeat([]byte("x"), 1<<20)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Attach(ctx, AttachOptions{SessionID: "7K3D", Conn: conn, In: input, Out: output})
		done <- err
	}()
	if err := reader.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("full-output cancellation = %v", err)
		}
	case <-time.After(time.Second):
		_ = reader.SetReadDeadline(time.Time{})
		drained := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, reader); close(drained) }()
		<-done
		_ = output.Close()
		<-drained
		t.Fatal("cancellation left output blocked until the pipe was drained")
	}
	after, err := term.GetState(input.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("output cancellation did not restore termios")
	}
	if _, err := output.Stat(); err != nil {
		t.Fatalf("attachment closed caller-owned output: %v", err)
	}
	if _, err := input.Stat(); err != nil {
		t.Fatalf("attachment closed caller-owned input: %v", err)
	}
	if got := goroutinesWithStack("internal/cli.relayInput"); got != 0 {
		t.Fatalf("input relay leaked: %d", got)
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	for _, message := range conn.controls {
		if message.Type == protocol.TypeDetach || message.Type == protocol.TypeSignal || message.Type == protocol.TypeKill {
			t.Fatalf("cancellation sent worker control: %s", message.Type)
		}
	}
}

func TestAttachRestorationToFullOutputIsBounded(t *testing.T) {
	master, input, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = input.Close() })
	before, err := term.GetState(input.Fd())
	if err != nil {
		t.Fatal(err)
	}
	reader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = output.Close() })
	if err := output.SetWriteDeadline(time.Now().Add(10 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write(bytes.Repeat([]byte("x"), 1<<20)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("fill output pipe: %v", err)
	}
	if err := output.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	conn := &outputCancelConn{frames: make(chan protocol.Frame, 2), closed: make(chan struct{})}
	conn.frames <- mustCommandControlFrame(protocol.Control{Type: protocol.TypeAttached, SessionID: "7K3D"})
	conn.frames <- mustCommandControlFrame(protocol.Control{Type: protocol.TypeExit, SessionID: "7K3D"})
	done := make(chan error, 1)
	go func() {
		_, err := Attach(t.Context(), AttachOptions{SessionID: "7K3D", Conn: conn, In: input, Out: output})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		drained := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, reader); close(drained) }()
		<-done
		_ = output.Close()
		<-drained
		t.Fatal("restoration escape output blocked attachment return")
	}
	after, err := term.GetState(input.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("restoration output prevented termios restoration")
	}
	if _, err := output.Stat(); err != nil {
		t.Fatalf("restoration closed caller-owned output: %v", err)
	}
}

type outputCancelConn struct {
	frames   chan protocol.Frame
	closed   chan struct{}
	once     sync.Once
	mu       sync.Mutex
	controls []protocol.Control
}

func (c *outputCancelConn) ReadFrame() (protocol.Frame, error) {
	select {
	case frame := <-c.frames:
		return frame, nil
	case <-c.closed:
		return protocol.Frame{}, net.ErrClosed
	}
}
func (c *outputCancelConn) WriteFrame(frame protocol.Frame) error {
	if frame.Kind == protocol.KindControl {
		message, err := protocol.DecodeControl(frame.Payload)
		if err != nil {
			return fmt.Errorf("decode test output control: %w", err)
		}
		c.mu.Lock()
		c.controls = append(c.controls, message)
		c.mu.Unlock()
	}
	return nil
}
func (c *outputCancelConn) Close() error { c.once.Do(func() { close(c.closed) }); return nil }
