package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/protocol"
)

const resumeTestSession = "7K3D"

// A retiring daemon can cut a reconnect while it authenticates. The same
// client must keep reconnecting and resume at its exact offset.
func TestReconnectResumesAfterAuthenticationCutByTheLink(t *testing.T) {
	for _, test := range []struct {
		name string
		cut  func(*Authentication) http.HandlerFunc
	}{
		{name: "retiring daemon cancels the handshake", cut: cutByCanceledRequest},
		{name: "link closes before acceptance", cut: cutBeforeAcceptance},
	} {
		t.Run(test.name, func(t *testing.T) {
			testReconnectAfterCut(t, test.cut)
		})
	}
}

func testReconnectAfterCut(t *testing.T, cut func(*Authentication) http.HandlerFunc) {
	logs := captureCloseLogs(t)
	server := newCutAuthServer(t)
	server.cut = cut(server.auth)
	conn := server.dial(t)
	writeResumeTestAttach(t, conn)
	readResumeTestControl(t, conn, protocol.TypeAttached)
	readResumeTestData(t, conn, "before")
	attached := readResumeTestControl(t, conn, protocol.TypeAttached)
	if attached.Seq != uint64(len("before")) {
		t.Fatalf("resumed attached at %d, want %d", attached.Seq, len("before"))
	}
	readResumeTestData(t, conn, "after")
	resume := <-server.resumes
	if resume.Type != protocol.TypeAttach || resume.LastSeq == nil || *resume.LastSeq != uint64(len("before")) {
		t.Fatalf("resume request = %+v, want attach at %d", resume, len("before"))
	}
	if generations := server.generations.Load(); generations != 3 {
		t.Fatalf("connections = %d, want original, cut and resumed", generations)
	}
	assertCloseLog(t, logs.String(), "stage=reconnect-dial", "reconnect=true", "stage=reconnect-established")
}

// A refusal is a peer decision. Retrying it would hide a revoked device.
func TestReconnectReturnsPeerRefusalWithoutRetry(t *testing.T) {
	logs := captureCloseLogs(t)
	server := newCutAuthServer(t)
	conn := server.dial(t)
	server.refuseAfterFirst.Store(true)
	writeResumeTestAttach(t, conn)
	readResumeTestControl(t, conn, protocol.TypeAttached)
	readResumeTestData(t, conn, "before")
	_, err := conn.ReadFrame()
	if !errors.Is(err, ErrAuthentication) || errors.Is(err, errAuthenticationInterrupted) {
		t.Fatalf("refused reconnect error = %v, want permanent authentication failure", err)
	}
	if !strings.Contains(err.Error(), "reconnect:") || !strings.Contains(err.Error(), "refused this device") {
		t.Fatalf("refused reconnect lost its stage: %v", err)
	}
	if generations := server.generations.Load(); generations != 2 {
		t.Fatalf("connections = %d, want no retry after refusal", generations)
	}
	assertCloseLog(t, logs.String(), "stage=reconnect-dial", "reconnect=false", "refused this device")
}

type cutAuthServer struct {
	*httptest.Server
	auth             *Authentication
	serverID         string
	clientAuth       *Authentication
	cut              http.HandlerFunc
	refuseAfterFirst atomic.Bool
	generations      atomic.Int32
	resumes          chan protocol.Control
}

func newCutAuthServer(t *testing.T) *cutAuthServer {
	t.Helper()
	serverID, serverKey := authIdentity(t)
	clientID, clientKey := authIdentity(t)
	server := &cutAuthServer{serverID: serverID, resumes: make(chan protocol.Control, 1),
		clientAuth: &Authentication{Key: clientKey, ExpectedIdentity: serverID}}
	server.auth = &Authentication{Key: serverKey, Authorize: func(id string) bool { return id == clientID },
		Bind: func(string) Authorization {
			current := !server.refuseAfterFirst.Load() || server.generations.Load() == 1
			return Authorization{Class: access.FullDevice, Current: func() bool { return current }}
		}}
	server.Server = httptest.NewServer(http.HandlerFunc(server.serveHTTP))
	t.Cleanup(server.Close)
	return server
}

func (s *cutAuthServer) dial(t *testing.T) Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, s.URL, DialOptions{Auth: s.clientAuth, Backoff: Backoff{Initial: 5 * time.Millisecond, Max: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func (s *cutAuthServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	generation := s.generations.Add(1)
	if generation == 2 && s.cut != nil {
		s.cut(w, r)
		return
	}
	_ = ServeWithOptions(w, r, ServeOptions{Auth: s.auth}, func(_ context.Context, conn Conn) error {
		if generation == 1 {
			return serveOriginalAttachment(conn)
		}
		return s.serveResumedAttachment(conn)
	})
}

func serveOriginalAttachment(conn Conn) error {
	if _, err := conn.ReadFrame(); err != nil {
		return fmt.Errorf("original attachment read: %w", err)
	}
	if err := writeResumeTestControl(conn, protocol.Control{Type: protocol.TypeAttached, SessionID: resumeTestSession}); err != nil {
		return err
	}
	// Returning closes the link, as a retiring daemon does to its attachments.
	if err := conn.WriteFrame(resumeTestData(0, "before")); err != nil {
		return fmt.Errorf("original attachment output: %w", err)
	}
	return nil
}

func (s *cutAuthServer) serveResumedAttachment(conn Conn) error {
	frame, err := conn.ReadFrame()
	if err != nil {
		return fmt.Errorf("resumed attachment read: %w", err)
	}
	resume, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("resumed attachment decode: %w", err)
	}
	s.resumes <- resume
	next := uint64(len("before"))
	if err := writeResumeTestControl(conn, protocol.Control{Type: protocol.TypeAttached, SessionID: resumeTestSession, Seq: next}); err != nil {
		return err
	}
	if err := conn.WriteFrame(resumeTestData(next, "after")); err != nil {
		return fmt.Errorf("resumed attachment output: %w", err)
	}
	if _, err := conn.ReadFrame(); err != nil {
		return fmt.Errorf("resumed attachment close: %w", err)
	}
	return nil
}

// cutByCanceledRequest is a daemon whose lifetime ended after it admitted the
// request: its authentication budget is already spent, so it closes the socket.
func cutByCanceledRequest(auth *Authentication) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		cancel()
		_ = ServeWithOptions(w, r.WithContext(ctx), ServeOptions{Auth: auth}, func(context.Context, Conn) error {
			return errors.New("canceled handshake dispatched a handler")
		})
	}
}

// cutBeforeAcceptance completes TLS and closes before the acceptance byte.
func cutBeforeAcceptance(auth *Authentication) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{AuthProtocol}})
		if err != nil {
			return
		}
		defer ws.CloseNow() //nolint:errcheck // the cut is the fixture
		cfg, err := auth.config(true)
		if err != nil {
			return
		}
		_ = tls.Server(websocket.NetConn(context.WithoutCancel(r.Context()), ws, websocket.MessageBinary), cfg).HandshakeContext(r.Context())
	}
}

func writeResumeTestAttach(t *testing.T, conn Conn) {
	t.Helper()
	if err := writeResumeTestControl(conn, protocol.Control{Type: protocol.TypeAttach, SessionID: resumeTestSession}); err != nil {
		t.Fatal(err)
	}
}

func writeResumeTestControl(conn Conn, control protocol.Control) error {
	payload, err := control.Encode()
	if err != nil {
		return fmt.Errorf("encode %s: %w", control.Type, err)
	}
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return fmt.Errorf("write %s: %w", control.Type, err)
	}
	return nil
}

func resumeTestData(seq uint64, payload string) protocol.Frame {
	sid, _ := protocol.NewSessionID(resumeTestSession)
	return protocol.Frame{Kind: protocol.KindData, Session: sid, Seq: seq, Payload: []byte(payload)}
}

func readResumeTestControl(t *testing.T, conn Conn, want string) protocol.Control {
	t.Helper()
	frame, err := conn.ReadFrame()
	if err != nil {
		t.Fatalf("read %s: %v", want, err)
	}
	control, err := protocol.DecodeControl(frame.Payload)
	if frame.Kind != protocol.KindControl || err != nil || control.Type != want {
		t.Fatalf("frame = kind %v %+v (%v), want %s", frame.Kind, control, err, want)
	}
	return control
}

func readResumeTestData(t *testing.T, conn Conn, want string) {
	t.Helper()
	frame, err := conn.ReadFrame()
	if err != nil {
		t.Fatalf("read output %q: %v", want, err)
	}
	if frame.Kind != protocol.KindData || string(frame.Payload) != want {
		t.Fatalf("frame = kind %v %q, want output %q", frame.Kind, frame.Payload, want)
	}
}
