package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transporttrace"
)

func TestServerLifecycleObservesApprovedOriginalAttachment(t *testing.T) {
	serverID, serverKey := authIdentity(t)
	clientID, clientKey := authIdentity(t)
	directory := t.TempDir()
	recorder := transporttrace.New(directory, transporttrace.Identity{PID: os.Getpid(), Version: "v0.1.9001",
		Commit: strings.Repeat("a", 40), Digest: strings.Repeat("b", 64)})
	t.Cleanup(recorder.Close)
	ctx := transporttrace.WithRecorder(t.Context(), recorder)
	done := make(chan error, 1)
	auth := &Authentication{Key: serverKey, Authorize: func(id string) bool { return id == clientID }}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, finish := transporttrace.Request(r)
		err := ServeWithOptions(w, r, ServeOptions{Auth: auth}, lifecycleAttachHandler)
		finish()
		done <- err
	}))
	server.Config.BaseContext = func(_ net.Listener) context.Context { return ctx }
	server.Start()
	t.Cleanup(server.Close)
	conn, err := DialOnce(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), DialOptions{Auth: &Authentication{Key: clientKey, ExpectedIdentity: serverID}})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := (protocol.Control{Type: protocol.TypeAttach, SessionID: "AB12"}).Encode()
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	frame, err := conn.ReadFrame()
	if err != nil || frame.Kind != protocol.KindControl {
		t.Fatalf("original attach reply = %#v, %v", frame, err)
	}
	_ = conn.Close()
	<-done
	closeLifecycleRecorder(t, recorder)
	files, _ := filepath.Glob(filepath.Join(directory, "*.jsonl"))
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"request.begin", "upgrade.done", "authorize.allow", "handshake.begin", "handshake.done", "grant.full", "grant.current", "acceptance.done", "stream.begin", "attach.read", "attached.write", "handler.end", "stream.close_begin", "stream.close_end", "request.end", "process.end"} {
		if !strings.Contains(string(data), `"stage":"`+stage+`"`) {
			t.Fatalf("missing lifecycle stage %s in %s", stage, data)
		}
	}
}

func TestServerLifecyclePreservesDenialAndCancellation(t *testing.T) {
	t.Run("denied", func(t *testing.T) { testLifecycleAuthenticationEnd(t, false) })
	t.Run("canceled", func(t *testing.T) { testLifecycleAuthenticationEnd(t, true) })
}

func testLifecycleAuthenticationEnd(t *testing.T, interrupt bool) {
	t.Helper()
	serverID, serverKey := authIdentity(t)
	_, clientKey := authIdentity(t)
	directory := t.TempDir()
	recorder := transporttrace.New(directory, transporttrace.Identity{PID: os.Getpid(), Version: "v0.1.9001",
		Commit: strings.Repeat("a", 40), Digest: strings.Repeat("b", 64)})
	t.Cleanup(recorder.Close)
	ctx, cancel := context.WithCancel(transporttrace.WithRecorder(t.Context(), recorder))
	t.Cleanup(cancel)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	auth := &Authentication{Key: serverKey, Authorize: func(string) bool { close(entered); <-release; return interrupt }}
	done := make(chan error, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, finish := transporttrace.Request(r)
		err := ServeWithOptions(w, r, ServeOptions{Auth: auth}, lifecycleAttachHandler)
		finish()
		done <- err
	}))
	server.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	server.Start()
	t.Cleanup(server.Close)
	t.Cleanup(unblock)
	dialed := make(chan error, 1)
	go func() {
		conn, err := DialOnce(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), DialOptions{Auth: &Authentication{Key: clientKey, ExpectedIdentity: serverID}})
		if conn != nil {
			_ = conn.Close()
		}
		dialed <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("authorizer was not reached")
	}
	if interrupt {
		cancel()
	} else {
		unblock()
	}
	err := <-dialed
	if interrupt {
		unblock()
	}
	<-done
	if !errors.Is(err, ErrAuthentication) {
		t.Fatalf("authentication result changed: %v", err)
	}
	closeLifecycleRecorder(t, recorder)
	files, _ := filepath.Glob(filepath.Join(directory, "*.jsonl"))
	data, _ := os.ReadFile(files[0])
	expected := `"stage":"authorize.deny"`
	if interrupt {
		expected = `"stage":"handshake.canceled"`
	}
	if !strings.Contains(string(data), expected) || strings.Contains(string(data), `"stage":"stream.begin"`) {
		t.Fatalf("missing failed authentication coverage: %s", data)
	}
}

func closeLifecycleRecorder(t *testing.T, recorder *transporttrace.Recorder) {
	t.Helper()
	recorder.Close()
	select {
	case <-recorder.Done():
	case <-time.After(time.Second):
		t.Fatal("lifecycle writer did not stop")
	}
}

func lifecycleAttachHandler(_ context.Context, conn Conn) error {
	frame, err := conn.ReadFrame()
	if err != nil {
		return fmt.Errorf("lifecycle test read attach: %w", err)
	}
	control, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return fmt.Errorf("lifecycle test decode attach: %w", err)
	}
	control.Type = protocol.TypeAttached
	payload, err := control.Encode()
	if err != nil {
		return fmt.Errorf("lifecycle test encode reply: %w", err)
	}
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return fmt.Errorf("lifecycle test write reply: %w", err)
	}
	_, err = conn.ReadFrame()
	if err != nil {
		return fmt.Errorf("lifecycle test wait for close: %w", err)
	}
	return nil
}

func TestServerLifecycleKeepsBoundGrantRejectionTerminal(t *testing.T) {
	calls := 0
	peer := AuthenticatedPeer{grant: Authorization{Current: func() bool { calls++; return false }}}
	err := confirmAuthentication(t.Context(), nil, nil, true, peer)
	if calls != 1 || err == nil || err.Error() != "transport: device grant removed during authentication" {
		t.Fatalf("grant rejection changed: calls=%d, error=%v", calls, err)
	}
}
