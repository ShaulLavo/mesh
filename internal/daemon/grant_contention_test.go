package daemon

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func TestPublishedGrantSurvivesOtherAndIdempotentWriterContention(t *testing.T) {
	state := t.TempDir()
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	actor, key, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{actor.ID, other.ID} {
		if err := identity.ApproveDevice(state, id); err != nil {
			t.Fatal(err)
		}
	}
	auth, err := controlAuthentication(state)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, echoControlFrames)
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, err := transport.DialOnce(ctx, server.URL, transport.DialOptions{Auth: &transport.Authentication{Key: key, ExpectedIdentity: host.ID}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	for _, action := range []struct {
		name   string
		mutate func() error
	}{
		{"idempotent other approval", func() error { return identity.ApproveDevice(state, other.ID) }},
		{"other revocation", func() error { return identity.RevokeDevice(state, other.ID) }},
		{"new other approval", func() error { return identity.ApproveDevice(state, other.ID) }},
		{"idempotent actor approval", func() error { return identity.ApproveDevice(state, actor.ID) }},
	} {
		lock, err := os.OpenFile(filepath.Join(state, "device-grants.lock"), os.O_RDWR, 0o600) //nolint:gosec // fixture-owned writer lock
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); _ = lock.Close() })
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		started, mutated := make(chan struct{}), make(chan error, 1)
		go func() { close(started); mutated <- action.mutate() }()
		<-started
		echo := make(chan error, 1)
		go func() { echo <- retainedGrantEcho(conn) }()
		select {
		case err := <-echo:
			if err != nil {
				t.Fatalf("%s retired actor: %v", action.name, err)
			}
		case <-ctx.Done():
			t.Fatalf("%s stalled admission while its writer lock was held", action.name)
		}
		select {
		case err := <-mutated:
			t.Fatalf("writer bypassed exclusive ownership: %v", err)
		default:
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-mutated:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("managed writer failed to settle")
		}
		if err := retainedGrantEcho(conn); err != nil {
			t.Fatalf("%s publication changed actor lifetime: %v", action.name, err)
		}
	}
}

func TestGrantContentionReleasesRealControlSlotBeforeUnlock(t *testing.T) {
	state := t.TempDir()
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	actor, key, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.ApproveDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	auth, err := controlAuthentication(state)
	if err != nil {
		t.Fatal(err)
	}
	entered, returned := make(chan struct{}), make(chan struct{})
	var authorizeOnce, returnedOnce sync.Once
	authorize := auth.Authorize
	auth.Authorize = func(id string) bool {
		authorizeOnce.Do(func() { close(entered) })
		return authorize(id)
	}
	connections := newConnectionGroup(echoControlFrames)
	connections.limit = 1
	boundary := newWebSocketServer(t.Context(), listenerConfig{webSocketPath: "/mesh", controlAuth: auth}, connections)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
		defer cancel()
		boundary.Handler.ServeHTTP(w, r.WithContext(ctx))
		returnedOnce.Do(func() { close(returned) })
	}))
	lock, err := os.OpenFile(filepath.Join(state, "device-grants.lock"), os.O_RDWR, 0o600) //nolint:gosec // fixture-owned policy lock
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); _ = lock.Close(); server.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	options := transport.DialOptions{Auth: &transport.Authentication{Key: key, ExpectedIdentity: host.ID}}
	dialed := make(chan struct{})
	go func() {
		conn, _ := transport.DialOnce(ctx, server.URL+"/mesh", options)
		if conn != nil {
			_ = conn.Close()
		}
		close(dialed)
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("fixture did not enter the real grant authorizer")
	}
	select {
	case <-returned:
	case <-ctx.Done():
		t.Fatal("canceled authentication retained the real control reservation behind a writer lock")
	}
	<-dialed
	conn, err := transport.DialOnce(ctx, server.URL+"/mesh", options)
	if err != nil {
		t.Fatalf("control slot was not reusable before policy unlock: %v", err)
	}
	defer func() { _ = conn.Close() }()
	assertControlEcho(t, conn)
}

func retainedGrantEcho(conn transport.Conn) error {
	payload := []byte(`{"type":"host.info"}`)
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return fmt.Errorf("write retained actor frame: %w", err)
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return fmt.Errorf("read retained actor frame: %w", err)
	}
	if frame.Kind != protocol.KindControl || !bytes.Equal(frame.Payload, payload) {
		return fmt.Errorf("actor control response changed")
	}
	return nil
}
