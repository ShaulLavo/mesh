package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/terminal"
	"github.com/shaul/mesh/internal/transport"
)

// An update stops the daemon an attachment uses. The client's first redial
// can reach that daemon after its lifetime ended, which cuts the redial
// mid-authentication. That cut must not end the attachment: the same client
// resumes on the successor at its exact offset and keeps terminal I/O.
func TestAttachResumesAfterRetiringDaemonCutsReconnectAuthentication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server := newRetiringDaemonServer(t)
	conn, err := transport.Dial(ctx, server.URL, transport.DialOptions{Auth: server.clientAuth,
		Backoff: transport.Backoff{Initial: 5 * time.Millisecond, Max: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	observed := &observedInputConn{Conn: conn, initial: make(chan struct{}), resumed: make(chan struct{})}
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()  //nolint:errcheck // test cleanup
	defer writer.Close() //nolint:errcheck // test cleanup
	local, err := localAttachInput(input, false)
	if err != nil {
		t.Fatal(err)
	}
	defer local.close() //nolint:errcheck // test cleanup
	var output bytes.Buffer
	type outcome struct {
		result AttachResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := AttachWithTerminal(ctx, AttachOptions{SessionID: "BBBB", Conn: observed, Raw: true}, AttachTerminal{
			Input: local.reader, Output: &output, Size: terminal.Size{Cols: 80, Rows: 24},
			CancelInput: local.cancel, ResetInput: local.reset,
		})
		done <- outcome{result, err}
	}()
	var ended outcome
	select {
	case <-observed.resumed:
	case ended = <-done:
		t.Fatalf("attachment ended before resuming: result=%+v, error=%v", ended.result, ended.err)
	case <-ctx.Done():
		t.Fatal("attachment did not resume")
	}
	writeAttachmentInput(t, writer, "fresh\n")
	select {
	case ended = <-done:
	case <-ctx.Done():
		t.Fatal("attachment did not finish after the resumed session exited")
	}
	if ended.err != nil {
		t.Fatalf("attachment ended with %v after a cut reconnect", ended.err)
	}
	if !ended.result.Exited || ended.result.Detached || ended.result.ExitCode != 0 {
		t.Fatalf("attachment result = %+v, want the resumed session's exit", ended.result)
	}
	if !strings.HasPrefix(output.String(), "before"+"after") {
		t.Fatalf("terminal output = %q, want original and resumed output in order", output.String())
	}
	if resume := server.resume.Load(); resume == nil || resume.LastSeq == nil || *resume.LastSeq != uint64(len("before")) {
		t.Fatalf("resume request = %+v, want exact offset %d", resume, len("before"))
	}
	if got := server.input.Load(); got == nil || *got != "fresh\n" {
		t.Fatalf("session input after resume = %v, want fresh input", got)
	}
	if generations := server.generations.Load(); generations != 3 {
		t.Fatalf("connections = %d, want original, cut and resumed", generations)
	}
}

type retiringDaemonServer struct {
	*httptest.Server
	auth        *transport.Authentication
	clientAuth  *transport.Authentication
	generations atomic.Int32
	resume      atomic.Pointer[protocol.Control]
	input       atomic.Pointer[string]
}

func newRetiringDaemonServer(t *testing.T) *retiringDaemonServer {
	t.Helper()
	serverID, serverKey := resumeAuthIdentity(t)
	clientID, clientKey := resumeAuthIdentity(t)
	server := &retiringDaemonServer{
		auth:       &transport.Authentication{Key: serverKey, Authorize: func(id string) bool { return id == clientID }},
		clientAuth: &transport.Authentication{Key: clientKey, ExpectedIdentity: serverID},
	}
	server.Server = httptest.NewServer(http.HandlerFunc(server.serveHTTP))
	t.Cleanup(server.Close)
	return server
}

func resumeAuthIdentity(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(public), private
}

func (s *retiringDaemonServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	generation := s.generations.Add(1)
	handler := s.serveResumed
	switch generation {
	case 1:
		handler = serveRetiringAttachment
	case 2:
		// The retiring daemon admitted this redial after its lifetime ended.
		ended, cancel := context.WithCancel(r.Context())
		cancel()
		r = r.WithContext(ended)
		handler = func(context.Context, transport.Conn) error {
			return errors.New("retired daemon dispatched a cut reconnect")
		}
	}
	_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: s.auth}, handler)
}

func serveRetiringAttachment(_ context.Context, conn transport.Conn) error {
	if _, err := conn.ReadFrame(); err != nil {
		return fmt.Errorf("retiring attachment read: %w", err)
	}
	// Returning closes the link, as a retiring daemon does to its attachments.
	return writeResumeFrames(conn, controlFrame(protocol.Control{Type: protocol.TypeAttached, SessionID: "BBBB"}),
		resumeOutputFrame(0, "before"))
}

func (s *retiringDaemonServer) serveResumed(_ context.Context, conn transport.Conn) error {
	frame, err := conn.ReadFrame()
	if err != nil {
		return fmt.Errorf("resumed attachment read: %w", err)
	}
	resume, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("resumed attachment decode: %w", err)
	}
	s.resume.Store(&resume)
	next := uint64(len("before"))
	if err := writeResumeFrames(conn, controlFrame(protocol.Control{Type: protocol.TypeAttached, SessionID: "BBBB", Seq: next})); err != nil {
		return err
	}
	if err := s.awaitInput(conn); err != nil {
		return err
	}
	code := 0
	if err := writeResumeFrames(conn, resumeOutputFrame(next, "after"),
		controlFrame(protocol.Control{Type: protocol.TypeExit, SessionID: "BBBB", ExitCode: &code})); err != nil {
		return err
	}
	if _, err := conn.ReadFrame(); err != nil {
		return fmt.Errorf("resumed attachment close: %w", err)
	}
	return nil
}

func writeResumeFrames(conn transport.Conn, frames ...protocol.Frame) error {
	for _, frame := range frames {
		if err := conn.WriteFrame(frame); err != nil {
			return fmt.Errorf("resume fixture write: %w", err)
		}
	}
	return nil
}

func (s *retiringDaemonServer) awaitInput(conn transport.Conn) error {
	for {
		frame, err := conn.ReadFrame()
		if err != nil {
			return fmt.Errorf("resumed attachment input: %w", err)
		}
		if frame.Kind == protocol.KindInput {
			payload := string(frame.Payload)
			s.input.Store(&payload)
			return nil
		}
	}
}

func resumeOutputFrame(seq uint64, payload string) protocol.Frame {
	sid, _ := protocol.NewSessionID("BBBB")
	return protocol.Frame{Kind: protocol.KindData, Session: sid, Seq: seq, Payload: []byte(payload)}
}
