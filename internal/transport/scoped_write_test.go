package transport

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/protocol"
)

func TestScopedCurrentIsCheckedInsideActualWriteLock(t *testing.T) {
	serverID, serverKey := authIdentity(t)
	clientID, clientKey := authIdentity(t)
	var current atomic.Bool
	current.Store(true)
	sockets := make(chan *socketConn, 1)
	send := make(chan struct{})
	completed := make(chan error, 1)
	var sendOnce sync.Once
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	t.Cleanup(func() { sendOnce.Do(func() { close(send) }) })
	auth := &Authentication{Key: serverKey, Authorize: func(id string) bool { return id == clientID }, Bind: func(string) Authorization {
		return Authorization{Class: access.Scoped, Current: current.Load, Admit: func(protocol.Frame) bool { return true }}
	}}
	handle := Handler(func(ctx context.Context, conn Conn) error {
		raw := conn.(*authorizedConn).Conn.(*BatchingConn).dst.(*socketConn)
		sockets <- raw
		select {
		case <-send:
		case <-ctx.Done():
			return nil
		}
		err := conn.WriteFrame(protocol.Frame{Kind: protocol.KindControl, Payload: []byte(`{"type":"access.read.result","secret":"queued"}`)})
		completed <- err
		return nil
	})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = ServeWithOptions(w, r, ServeOptions{Auth: auth}, handle)
	}))
	t.Cleanup(server.Close)
	server.Start()
	conn, err := DialOnce(ctx, server.URL, DialOptions{Auth: &Authentication{Key: clientKey, ExpectedIdentity: serverID, Admission: ScopedAdmission}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var socket *socketConn
	select {
	case socket = <-sockets:
	case <-ctx.Done():
		t.Fatal("server socket not published")
	}
	socket.writeMu.Lock()
	var unlock sync.Once
	defer unlock.Do(socket.writeMu.Unlock)
	native := socket.writeCurrent
	var checks atomic.Int32
	socket.writeCurrent = func() bool { checks.Add(1); return native() }
	sendOnce.Do(func() { close(send) })
	current.Store(false)
	unlock.Do(socket.writeMu.Unlock)
	select {
	case err := <-completed:
		if err == nil {
			t.Fatal("queued reply wrote after invalidation")
		}
	case <-ctx.Done():
		t.Fatal("queued writer did not finish")
	}
	if checks.Load() == 0 {
		t.Fatal("eventual socket write bypassed the scoped current predicate")
	}
	if frame, err := conn.ReadFrame(); err == nil {
		t.Fatalf("queued protected bytes escaped: %s", frame.Payload)
	}
}
func TestAdmissionSetsPreserveOrdinarySignedUpdaterAndScoped(t *testing.T) {
	for _, test := range []struct {
		set   Admission
		class byte
		want  bool
	}{
		{FullAdmission, 1, true}, {FullAdmission, 2, false}, {FullAdmission, 3, false},
		{SignedUpdateAdmission, 1, true}, {SignedUpdateAdmission, 2, true}, {SignedUpdateAdmission, 3, false},
		{ScopedAdmission, 1, false}, {ScopedAdmission, 2, false}, {ScopedAdmission, 3, true},
		{ScopedAdmission, 0, false}, {FullAdmission, 9, false},
	} {
		t.Run(fmt.Sprintf("%d/%d", test.set, test.class), func(t *testing.T) {
			checkAdmissionCase(t, test.set, test.class, test.want)
		})
	}
}

func checkAdmissionCase(t *testing.T, set Admission, class byte, want bool) {
	t.Helper()
	if set.accepts(class) != want {
		t.Fatal("wrong accepted class set")
	}
}
