package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/worker"
)

func TestAttachNestingRestoresDetachAfterInnerConnectionCloses(t *testing.T) {
	client := startNestingAttachment(t, AttachOptions{})
	client.update(protocol.Control{
		Type: protocol.TypeAttached, NestingSupported: true,
		Nested: []protocol.SessionIdentity{{HostID: "host-b", SessionID: "BBBB"}},
	})
	client.forward([]byte{'a', DefaultDetachKey, 'b'})
	client.update(protocol.Control{Type: protocol.TypeNesting, NestingSupported: true})
	client.detach(DefaultDetachKey)
}

func TestAttachLeaveKeyRequiresRegisteredInnerClient(t *testing.T) {
	for _, test := range []struct {
		name       string
		supported  bool
		registered bool
	}{
		{name: "legacy worker"},
		{name: "legacy inner client", supported: true},
		{name: "registered inner client", supported: true, registered: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := startNestingAttachment(t, AttachOptions{})
			message := protocol.Control{Type: protocol.TypeAttached, NestingSupported: test.supported}
			if test.registered {
				message.Nested = []protocol.SessionIdentity{{HostID: "host-b", SessionID: "BBBB"}}
			}
			client.update(message)
			if test.registered {
				client.detach(DefaultLeaveKey)
			} else {
				client.forward([]byte{DefaultLeaveKey})
				client.detach(DefaultDetachKey)
			}
		})
	}
}

func TestAttachCustomKeysAndRawKeepTheirMeaning(t *testing.T) {
	for _, test := range []struct {
		name    string
		opts    AttachOptions
		forward []byte
		detach  byte
	}{
		{name: "explicit detach", opts: AttachOptions{DetachKey: 0x02, DetachKeyExplicit: true}, forward: []byte{DefaultDetachKey}, detach: 0x02},
		{name: "explicit default detach", opts: AttachOptions{DetachKey: DefaultDetachKey, DetachKeyExplicit: true}, detach: DefaultDetachKey},
		{name: "custom leave", opts: AttachOptions{LeaveKey: 0x1c}, forward: []byte{DefaultLeaveKey}, detach: 0x1c},
		{name: "disabled leave", opts: AttachOptions{DisableLeaveKey: true}, forward: []byte{DefaultDetachKey, DefaultLeaveKey}},
		{name: "raw", opts: AttachOptions{Raw: true}, forward: []byte{DefaultDetachKey, DefaultLeaveKey}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := startNestingAttachment(t, test.opts)
			client.update(protocol.Control{
				Type: protocol.TypeAttached, NestingSupported: true,
				Nested: []protocol.SessionIdentity{{HostID: "host-b", SessionID: "BBBB"}},
			})
			if len(test.forward) > 0 {
				client.forward(test.forward)
			}
			if test.detach != 0 {
				client.detach(test.detach)
			} else {
				client.stop()
			}
		})
	}
}

func TestRegisteredInnerAttachmentUsesDefaultKeyEvenForLegacyDestination(t *testing.T) {
	keys := newAttachmentKeys(AttachOptions{DetachKey: DetachKeyForDepth(1)}, 1, true, true)
	if err := keys.update(protocol.Control{Type: protocol.TypeAttached}); err != nil {
		t.Fatal(err)
	}
	if got := keys.detachIndex([]byte{DefaultLeaveKey, DefaultDetachKey}); got != 1 {
		t.Fatalf("inner detach index = %d; want ctrl+] after forwarding ctrl+^", got)
	}
	legacy := newAttachmentKeys(AttachOptions{DetachKey: DetachKeyForDepth(1)}, 1, true, false)
	if got := legacy.detachIndex([]byte{DefaultDetachKey, DefaultLeaveKey}); got != 1 {
		t.Fatalf("legacy inner detach index = %d; want depth-1 ctrl+^", got)
	}
}

func TestFallbackAttachmentKeepsItsDepthKeyWhenDestinationHasNestedClients(t *testing.T) {
	keys := newAttachmentKeys(AttachOptions{}, 1, true, false)
	if err := keys.update(protocol.Control{
		Type: protocol.TypeAttached, NestingSupported: true,
		Nested: []protocol.SessionIdentity{{HostID: "host-c", SessionID: "CCCC"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := keys.detachIndex([]byte{DefaultDetachKey, DefaultLeaveKey}); got != 1 {
		t.Fatalf("fallback detach index = %d, want depth-1 ctrl+^", got)
	}
}

func TestAttachmentAdvertisesDynamicKeysOnlyWhenItCanForwardThem(t *testing.T) {
	for _, test := range []struct {
		name string
		opts AttachOptions
		want bool
	}{
		{name: "default", want: true},
		{name: "explicit default", opts: AttachOptions{DetachKey: DefaultDetachKey, DetachKeyExplicit: true}},
		{name: "explicit depth key", opts: AttachOptions{DetachKey: DefaultLeaveKey, DetachKeyExplicit: true}, want: true},
		{name: "raw explicit default", opts: AttachOptions{DetachKey: DefaultDetachKey, DetachKeyExplicit: true, Raw: true}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := startNestingAttachment(t, test.opts)
			if client.request.NestingSupported != test.want {
				t.Errorf("attach nesting capability = %t, want %t", client.request.NestingSupported, test.want)
			}
			client.stop()
		})
	}
}

func TestNestingCapabilityFollowsTheEntireUpstreamChain(t *testing.T) {
	for _, registered := range []bool{false, true} {
		keys := newAttachmentKeys(AttachOptions{}, 2, true, registered)
		if keys.dynamic != registered {
			t.Fatalf("registered=%t advertised dynamic keys=%t", registered, keys.dynamic)
		}
	}
}

func TestAttachDetachedRefusalNeverFallsBackToStealing(t *testing.T) {
	for _, test := range []struct {
		name    string
		reason  string
		message string
		want    error
	}{
		{name: "already attached", reason: protocol.ReasonAttached, message: "session is already attached", want: ErrSessionAttached},
		{name: "legacy worker", message: "expected " + protocol.TypeAttach, want: ErrAttachDetachedUnsupported},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := startNestingAttachment(t, AttachOptions{IfDetached: true})
			if client.request.Type != protocol.TypeAttachDetached {
				t.Fatalf("request type = %q, want detached-only attach", client.request.Type)
			}
			if err := client.writer.WriteControlMsg(protocol.Control{Type: protocol.TypeError, Reason: test.reason, Message: test.message}); err != nil {
				t.Fatal(err)
			}
			result := client.wait()
			if !errors.Is(result.err, test.want) {
				t.Fatalf("attach error = %v, want %v", result.err, test.want)
			}
			if _, err := client.reader.ReadFrame(); !errors.Is(err, io.EOF) {
				t.Fatalf("frame after refusal error = %v, want closed connection without retry", err)
			}
		})
	}
}

func TestAttachDetachedDisappearingCandidateIsUnavailable(t *testing.T) {
	client := startNestingAttachment(t, AttachOptions{IfDetached: true})
	if err := client.conn.Close(); err != nil {
		t.Fatal(err)
	}
	if result := client.wait(); !errors.Is(result.err, ErrSessionUnavailable) {
		t.Fatalf("unacknowledged claim error = %v, want unavailable", result.err)
	}
}

func TestNestingRegistrationHoldsConnectionAndRejectsLegacyWorkers(t *testing.T) {
	for _, supported := range []bool{false, true} {
		name := "legacy"
		if supported {
			name = "supported"
		}
		t.Run(name, func(t *testing.T) {
			location := worker.SessionWorkerLocation{SessionID: "AAAA", Dir: t.TempDir()}
			listener, err := net.Listen("unix", paths.Socket(location.Dir))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			target := protocol.SessionIdentity{HostID: "remote-host", SessionID: "BBBB"}
			accepted := make(chan net.Conn, 1)
			requests := make(chan protocol.Control, 1)
			closed := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					closed <- err
					return
				}
				defer conn.Close() //nolint:errcheck // test resource cleanup
				accepted <- conn
				reader := protocol.NewReader(conn)
				frame, err := reader.ReadFrame()
				if err != nil {
					closed <- err
					return
				}
				request, err := protocol.DecodeControl(frame.Payload)
				if err != nil {
					closed <- err
					return
				}
				requests <- request
				response := protocol.Control{Type: protocol.TypeError, RequestID: request.RequestID, SessionID: location.SessionID}
				if supported {
					response.Type = protocol.TypeNesting
					response.NestingSupported = true
					response.Nested = []protocol.SessionIdentity{target}
				}
				if err := protocol.NewWriter(conn).WriteControlMsg(response); err != nil {
					closed <- err
					return
				}
				_, err = reader.ReadFrame()
				closed <- err
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			registration, err := registerNesting(ctx, location, target)
			cancel()
			server := <-accepted
			t.Cleanup(func() { _ = server.Close() })
			request := <-requests
			if request.Type != protocol.TypeNest || request.SessionID != location.SessionID || request.NestedSession == nil || *request.NestedSession != target {
				t.Fatalf("registration request = %#v", request)
			}
			if supported {
				if err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-closed:
					t.Fatalf("registration closed when request context ended: %v", err)
				default:
				}
				if err := registration.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err == nil || registration != nil {
				t.Fatalf("legacy registration = %v, %v; want unsupported error", registration, err)
			}
			select {
			case err := <-closed:
				if !errors.Is(err, io.EOF) {
					t.Fatalf("registration close error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("registration connection was left open")
			}
		})
	}
}

func TestAttachmentTargetDoesNotInferRemoteHostFromEnvironment(t *testing.T) {
	t.Setenv(worker.MeshHostIDVariable, "outer-host")
	if _, err := attachmentTargetIdentity(AttachOptions{SessionID: "BBBB"}); err == nil {
		t.Fatal("remote attachment inherited the local host identity")
	}
	stateDir := t.TempDir()
	local, _, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := attachmentTargetIdentity(AttachOptions{SessionID: "BBBB", SocketPath: paths.Socket(filepath.Join(stateDir, "s", "BBBB"))})
	if err != nil || got.HostID != local.ID {
		t.Fatalf("local target = %#v, %v; want host %s", got, err, local.ID)
	}
}

func TestLegacyDetachHintDoesNotContaminateNonTerminalOutput(t *testing.T) {
	t.Setenv("MESH_DEPTH", "1")
	containing := []protocol.SessionIdentity{{HostID: "ancestor-host", SessionID: "AAAA"}}
	if location, inside := worker.ContainingSessionWorker(); inside {
		containing = legacyContainingSession(location, os.Getenv(worker.MeshSessionIDVariable), os.Getenv(worker.MeshHostIDVariable), identity.Load)
		if len(containing) == 0 {
			t.Fatal("cannot identify the test process's containing worker")
		}
	}
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	if err := server.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err := transport.NewStreamConn(client)
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close() //nolint:errcheck // test resource cleanup
	output, err := os.CreateTemp(t.TempDir(), "stream")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close() //nolint:errcheck // test resource cleanup
	var diagnostics bytes.Buffer
	done := make(chan error, 1)
	go func() {
		// With no destination identity, this embedded caller uses legacy keys
		// without registering against the terminal that runs the test suite.
		_, err := Attach(AttachOptions{
			Conn: conn, SessionID: "BBBB", ContainingSessions: containing,
			In: input, Out: output, Stderr: &diagnostics,
		})
		done <- err
	}()
	if _, err := protocol.NewReader(server).ReadFrame(); err != nil {
		t.Fatal(err)
	}
	writer := protocol.NewWriter(server)
	if err := writer.WriteControlMsg(protocol.Control{Type: protocol.TypeAttached, SessionID: "BBBB"}); err != nil {
		t.Fatal(err)
	}
	sid, _ := protocol.NewSessionID("BBBB")
	want := []byte("binary\x00output\r\nFINAL_MARKER")
	if err := writer.WriteData(sid, 0, want); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteControlMsg(protocol.Control{Type: protocol.TypeExit, SessionID: "BBBB"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("attachment did not finish after session exit")
	}
	got, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("session output = %q, want exact stream %q", got, want)
	}
	if !bytes.Contains(diagnostics.Bytes(), []byte("ctrl+^ detaches this one")) {
		t.Fatalf("stderr = %q, want the legacy detach hint", diagnostics.String())
	}
}

type nestingAttachmentResult struct {
	result AttachResult
	err    error
}

type nestingAttachment struct {
	t       *testing.T
	conn    net.Conn
	input   *os.File
	output  *os.File
	reader  *protocol.Reader
	writer  *protocol.Writer
	request protocol.Control
	done    chan nestingAttachmentResult
}

func startNestingAttachment(t *testing.T, opts AttachOptions) *nestingAttachment {
	t.Helper()
	t.Setenv("MESH_DEPTH", "0")
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	if err := server.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err := transport.NewStreamConn(client)
	if err != nil {
		t.Fatal(err)
	}
	in, input, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	output, out, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := output.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close(); _ = input.Close(); _ = out.Close(); _ = output.Close() })
	opts.Conn, opts.SessionID, opts.In, opts.Out = conn, "AAAA", in, out
	done := make(chan nestingAttachmentResult, 1)
	go func() {
		result, err := Attach(opts)
		done <- nestingAttachmentResult{result: result, err: err}
	}()
	reader := protocol.NewReader(server)
	frame, err := reader.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	request, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return &nestingAttachment{t: t, conn: server, input: input, output: output, reader: reader, writer: protocol.NewWriter(server), request: request, done: done}
}

func (client *nestingAttachment) update(message protocol.Control) {
	client.t.Helper()
	message.SessionID = "AAAA"
	if err := client.writer.WriteControlMsg(message); err != nil {
		client.t.Fatal(err)
	}
	sid, _ := protocol.NewSessionID("AAAA")
	if err := client.writer.WriteData(sid, 0, []byte{'!'}); err != nil {
		client.t.Fatal(err)
	}
	// Observing output after the control frame proves the read loop has
	// installed the key state before the test supplies its next keystroke.
	marker := make([]byte, 1)
	if _, err := io.ReadFull(client.output, marker); err != nil {
		client.t.Fatal(err)
	}
}

func (client *nestingAttachment) forward(input []byte) {
	client.t.Helper()
	if _, err := client.input.Write(input); err != nil {
		client.t.Fatal(err)
	}
	frame, err := client.reader.ReadFrame()
	if err != nil {
		client.t.Fatal(err)
	}
	if frame.Kind != protocol.KindInput || !bytes.Equal(frame.Payload, input) {
		client.t.Fatalf("forwarded frame = %#v, want input %q", frame, input)
	}
}

func (client *nestingAttachment) detach(key byte) {
	client.t.Helper()
	if _, err := client.input.Write([]byte{key}); err != nil {
		client.t.Fatal(err)
	}
	frame, err := client.reader.ReadFrame()
	if err != nil {
		client.t.Fatal(err)
	}
	message, err := protocol.DecodeControl(frame.Payload)
	if frame.Kind != protocol.KindControl || err != nil || message.Type != protocol.TypeDetach {
		client.t.Fatalf("detach frame = %#v, %v", frame, err)
	}
	result := client.wait()
	if result.err != nil || !result.result.Detached {
		client.t.Fatalf("detach result = %#v, %v", result.result, result.err)
	}
}

func (client *nestingAttachment) stop() {
	client.t.Helper()
	if err := client.writer.WriteControlMsg(protocol.Control{Type: protocol.TypeDetach, SessionID: "AAAA"}); err != nil {
		client.t.Fatal(err)
	}
	if result := client.wait(); result.err != nil {
		client.t.Fatal(result.err)
	}
}

func (client *nestingAttachment) wait() nestingAttachmentResult {
	client.t.Helper()
	select {
	case result := <-client.done:
		return result
	case <-time.After(3 * time.Second):
		client.t.Fatal("attachment did not return")
		return nestingAttachmentResult{}
	}
}
