package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/shaul/mesh/internal/protocol"
)

func captureCloseLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &output
}

func TestTransportCloseLogsClientHandshakeEOF(t *testing.T) {
	logs := captureCloseLogs(t)
	checkClientHandshakeEOF(t)
	assertCloseLog(t, logs.String(), "server=false", "stage=tls-handshake", "classification=link-failed", "TLS handshake:", "EOF")
}

func checkClientHandshakeEOF(t *testing.T) {
	t.Helper()
	serverID, _ := authIdentity(t)
	_, key := authIdentity(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{AuthProtocol}})
		if err == nil {
			_ = ws.CloseNow()
		}
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	conn, err := DialOnce(ctx, server.URL, DialOptions{Auth: &Authentication{Key: key, ExpectedIdentity: serverID}})
	if conn != nil || !errors.Is(err, ErrAuthentication) || !errors.Is(err, io.EOF) {
		t.Fatalf("handshake outcome = %v, %v", conn, err)
	}
}

func TestTransportCloseLogsDaemonHandshakeEOF(t *testing.T) {
	logs := captureCloseLogs(t)
	_, key := authIdentity(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	result := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result <- ServeWithOptions(w, r.WithContext(ctx), ServeOptions{Auth: &Authentication{Key: key, Authorize: func(string) bool { return true }}}, func(context.Context, Conn) error {
			return errors.New("closed handshake reached handler")
		})
	}))
	t.Cleanup(server.Close)
	ws, _, err := websocket.Dial(ctx, server.URL, &websocket.DialOptions{Subprotocols: []string{AuthProtocol}}) //nolint:bodyclose // websocket owns the response body
	if err != nil {
		t.Fatal(err)
	}
	_ = ws.CloseNow()
	err = waitCloseLogResult(ctx, t, result)
	if !errors.Is(err, ErrAuthentication) || !errors.Is(err, io.EOF) {
		t.Fatalf("daemon handshake outcome = %v", err)
	}
	assertCloseLog(t, logs.String(), "server=true", "stage=tls-handshake", "classification=link-failed", "TLS handshake:", "EOF")
}

func TestTransportCloseLogsEstablishedEOFWithoutTerminalPayload(t *testing.T) {
	logs := captureCloseLogs(t)
	payload := []byte("private terminal content")
	sid, err := protocol.NewSessionID("7K3D")
	if err != nil {
		t.Fatal(err)
	}
	frame := protocol.Frame{Kind: protocol.KindData, Session: sid, Seq: 7, Payload: payload}
	result := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			result <- err
			return
		}
		encoded, err := encodeFrame(frame)
		if err == nil {
			err = ws.Write(r.Context(), websocket.MessageBinary, encoded)
		}
		_ = ws.CloseNow()
		result <- err
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	conn, err := DialOnce(ctx, server.URL, DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	got, err := conn.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	assertFrameEqual(t, got, frame)
	_, err = conn.ReadFrame()
	if !errors.Is(err, io.EOF) || err != conn.(*socketConn).terminalError() { //nolint:errorlint // logging must preserve the exact stored error, including its wrappers
		t.Fatalf("stream error identity lost: %v", err)
	}
	_, repeated := conn.ReadFrame()
	if repeated != err { //nolint:errorlint // repeated reads must return the same stored error
		t.Fatalf("repeated read changed terminal cause: %v, %v", err, repeated)
	}
	if err := waitCloseLogResult(ctx, t, result); err != nil {
		t.Fatal(err)
	}
	assertCloseLog(t, logs.String(), "stage=established-stream", "classification=link-failed", "read WebSocket frame:", "EOF")
	if strings.Contains(logs.String(), string(payload)) {
		t.Fatal("transport logs contain terminal payload")
	}
	if strings.Count(logs.String(), "stage=established-stream") != 1 {
		t.Fatalf("stream failure logged more than once: %q", logs.String())
	}
}

func TestTransportCloseLogsResumeWriteAndSuccessfulReplacement(t *testing.T) {
	logs := captureCloseLogs(t)
	old, next := newTestLink(false), newTestLink(false)
	conn := reconnectTestConnection(t, old, next)
	writeTestControl(t, conn, protocol.Control{Type: protocol.TypeAttach, SessionID: "7K3D"})
	cause := fmt.Errorf("resume write: %w", io.ErrClosedPipe)
	failed := &closeLogWriteFailure{testLink: newTestLink(false), err: cause}
	attempts := 0
	conn.dial = func(context.Context, string, websocket.DialOptions, KeepAlive) (linkConn, error) {
		attempts++
		if attempts == 1 {
			return failed, nil
		}
		return next, nil
	}
	if err := conn.sendResumes(failed); !errors.Is(err, cause) {
		t.Fatalf("resume error identity lost: %v", err)
	}
	ref, err := conn.connection(conn.current)
	if err != nil || ref.generation != 2 || attempts != 2 || !failed.stopped() || len(next.snapshot()) != 1 {
		t.Fatalf("replacement outcome = %v, %v, attempts=%d", ref, err, attempts)
	}
	assertCloseLog(t, logs.String(), "stage=resume-write", "reconnect=true", "resume sessions: resume write: io: read/write on closed pipe", "stage=reconnect-established", "generation=2")
}

type closeLogWriteFailure struct {
	*testLink
	err error
}

func (c *closeLogWriteFailure) WriteFrame(protocol.Frame) error { return c.err }

func waitCloseLogResult(ctx context.Context, t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		t.Fatal("transport control did not finish")
		return fmt.Errorf("wait for transport control: %w", ctx.Err())
	}
}

func assertCloseLog(t *testing.T, output string, fields ...string) {
	t.Helper()
	for _, field := range fields {
		if !strings.Contains(output, field) {
			t.Errorf("log %q missing %q", output, field)
		}
	}
}
