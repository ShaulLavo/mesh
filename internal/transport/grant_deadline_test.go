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
	admission := make(chan struct{})
	returned := make(chan struct{})
	var enteredOnce sync.Once
	var admissionOnce sync.Once
	release := func() { admissionOnce.Do(func() { close(admission) }) }
	var dispatched bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		auth := &Authentication{Key: hostKey, Authorize: func(id string) bool {
			enteredOnce.Do(func() { close(entered) })
			<-admission
			// Cancel at grant admission so TLS setup cannot spend the fixture's cancellation budget.
			cancel()
			return identity.GrantedIdentity(state, id)
		}}
		_ = ServeWithOptions(w, r.WithContext(ctx), ServeOptions{Auth: auth}, func(context.Context, Conn) error {
			dispatched = true
			return errors.New("fixture should not dispatch")
		})
		close(returned)
	}))
	// Release the policy lock before server.Close, even when the assertion fails.
	defer func() { release(); _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); server.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
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
	case err := <-dial:
		t.Fatalf("fixture failed before reaching locked authorizer: %v", err)
	case <-ctx.Done():
		t.Fatal("fixture never reached locked authorizer")
	}
	// A published approval remains valid while a writer holds its own mutation lock.
	// Cancellation must release both peer and handler before unlock.
	completion, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	release()
	select {
	case err := <-dial:
		if !errors.Is(err, ErrAuthentication) {
			t.Fatalf("canceled grant admission returned %v; want authentication failure", err)
		}
	case <-completion.Done():
		t.Fatal("handshake cancellation retained peer behind locked authorizer")
	}
	select {
	case <-returned:
	case <-completion.Done():
		t.Fatal("handshake cancellation closed peer but retained server handler/control slot behind blocking grant flock")
	}
	if dispatched {
		t.Fatal("canceled grant admission dispatched a handler")
	}
}
