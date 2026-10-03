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
	"github.com/shaul/mesh/internal/update"
)

func updaterPolicyAuthentication(t *testing.T) (string, *transport.Authentication, transport.DialOptions) {
	t.Helper()
	state := t.TempDir()
	host, _, err := identity.LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	actor, key, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := update.Trust(state, actor.ID, true); err != nil {
		t.Fatal(err)
	}
	auth, err := controlAuthentication(state)
	if err != nil {
		t.Fatal(err)
	}
	return state, auth, transport.DialOptions{Auth: &transport.Authentication{
		Key: key, ExpectedIdentity: host.ID, AllowUpdateOnly: true,
	}}
}

func updaterPolicyServer(t *testing.T, auth *transport.Authentication) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = transport.ServeWithOptions(w, r, transport.ServeOptions{Auth: auth}, echoControlFrames)
	}))
	t.Cleanup(server.Close)
	return server
}

func assertUpdaterPolicyAccepted(t *testing.T, server *httptest.Server, options transport.DialOptions) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	conn, err := transport.DialOnce(ctx, server.URL, options)
	if err != nil {
		t.Fatal("known-good updater-only TLS authentication rejected")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdaterPolicyFIFOReleasesRealControlSlot(t *testing.T) {
	state, auth, options := updaterPolicyAuthentication(t)
	assertUpdaterPolicyAccepted(t, updaterPolicyServer(t, auth), options)
	path := filepath.Join(state, "updates", "administrators.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
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
		if r.Header.Get("Sec-WebSocket-Protocol") == transport.AuthProtocol {
			returnedOnce.Do(func() { close(returned) })
		}
	}))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	dialed := make(chan struct{})
	go func() {
		conn, _ := transport.DialOnce(ctx, server.URL+"/mesh", options)
		if conn != nil {
			_ = conn.Close()
		}
		close(dialed)
	}()
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0600) //nolint:gosec // fixture-only FIFO release before joining the real handler
			if err == nil {
				_, _ = writer.Write([]byte("{}"))
				_ = writer.Close()
			}
		})
	}
	defer func() { release(); cancel(); server.Close(); <-dialed }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("fixture did not enter the real updater authorizer")
	}
	select {
	case <-returned:
	case <-ctx.Done():
		t.Error("updater policy FIFO retained handler and control reservation after cancellation")
	}
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(server.URL + "/mesh")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode == http.StatusServiceUnavailable {
		t.Error("updater policy FIFO kept the one-slot control reservation occupied")
	}
	release()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("handler did not settle after controlled FIFO release")
	}
	<-dialed
}

func TestUpdaterPolicyUnsafeSnapshotsRejectRealTLS(t *testing.T) {
	for _, name := range []string{"missing", "symlink", "world writable", "group writable", "unreadable", "directory", "oversized"} {
		t.Run(name, func(t *testing.T) {
			state, auth, options := updaterPolicyAuthentication(t)
			server := updaterPolicyServer(t, auth)
			assertUpdaterPolicyAccepted(t, server, options)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			current, err := transport.DialOnce(ctx, server.URL, options)
			if err != nil {
				t.Fatal("known-good established updater socket rejected")
			}
			t.Cleanup(func() { _ = current.Close() })
			if err := updaterPolicyEcho(current); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(state, "updates", "administrators.json")
			switch name {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(state, "other-policy.json")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "world writable", "group writable", "unreadable":
				mode := map[string]os.FileMode{"world writable": 0666, "group writable": 0620, "unreadable": 0000}[name]
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				file, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // fixture-owned policy
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate((2 << 20) + 1); err != nil {
					_ = file.Close()
					t.Fatal(err)
				}
				_ = file.Close()
			}
			conn, err := transport.DialOnce(ctx, server.URL, options)
			if conn != nil {
				_ = conn.Close()
			}
			if err == nil {
				t.Error("unsafe updater policy accepted through real TLS authentication")
			}
			if !auth.Authorize(options.Auth.ExpectedIdentity) {
				t.Error("local host lost its own updater authority")
			}
			retired, joined := make(chan error, 1), make(chan struct{})
			go func() { defer close(joined); retired <- updaterPolicyEcho(current) }()
			t.Cleanup(func() { _ = current.Close(); <-joined })
			select {
			case err := <-retired:
				if err == nil {
					t.Error("unsafe policy retained the established updater scope")
				}
			case <-ctx.Done():
				t.Error("unsafe policy stalled the established updater scope")
			}
		})
	}
}

func updaterPolicyEcho(conn transport.Conn) error {
	payload := []byte(`{"type":"update.control"}`)
	if err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: payload}); err != nil {
		return fmt.Errorf("write updater scope probe: %w", err)
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return fmt.Errorf("read updater scope probe: %w", err)
	}
	if frame.Kind != protocol.KindControl || !bytes.Equal(frame.Payload, payload) {
		return fmt.Errorf("updater scope probe response changed")
	}
	return nil
}
