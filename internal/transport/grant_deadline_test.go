package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/identity"
)

func TestGrantLockMustRespectHandshakeCancellation(t *testing.T) {
	state := t.TempDir()
	actor, key, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host, hostKey, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.ApproveDevice(state, actor.ID); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(state, "device-grants.lock"), os.O_RDWR, 0600) //nolint:gosec // fixture policy lock beneath t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	returned := make(chan struct{})
	var enteredOnce sync.Once
	auth := &Authentication{Key: hostKey, Authorize: func(id string) bool {
		enteredOnce.Do(func() { close(entered) })
		return identity.GrantedIdentity(state, id)
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
		defer cancel()
		_ = ServeWithOptions(w, r.WithContext(ctx), ServeOptions{Auth: auth}, func(context.Context, Conn) error { return errors.New("fixture should not dispatch") })
		close(returned)
	}))
	// Release the policy lock before server.Close, even when the assertion fails.
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); server.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	dial := make(chan error, 1)
	go func() {
		conn, err := DialOnce(ctx, server.URL, DialOptions{Auth: &Authentication{Key: key, ExpectedIdentity: host.ID}})
		if conn != nil {
			_ = conn.Close()
		}
		dial <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("fixture never reached locked authorizer")
	}
	// A published approval remains valid while a writer holds its own mutation lock.
	// Admission may succeed or cancel, but it must release the handler before unlock.
	<-dial
	select {
	case <-returned:
	case <-ctx.Done():
		t.Fatal("handshake cancellation closed peer but retained server handler/control slot behind blocking grant flock")
	}
}
