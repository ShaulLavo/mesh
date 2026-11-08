package sshd

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// NewServerConn has finished when Charm clears its handshake deadline, but
// Charm has not registered the authenticated connection for Server.Close yet.
type registrationGapConn struct {
	net.Conn
	clears int
	paused chan struct{}
	resume chan struct{}
}

func (c *registrationGapConn) SetDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		c.clears++
	}
	if c.clears == 2 && deadline.IsZero() {
		close(c.paused)
		<-c.resume
	}
	return c.Conn.SetDeadline(deadline) //nolint:wrapcheck // preserve net.Conn errors
}

func TestListenerShutdownClosesConnectionBeforeSSHRegistration(t *testing.T) {
	private := generatePrivateKey(t)
	host := generatePrivateKey(t)
	server := mustServer(t, Config{ScopeMembership: fixtureNoScopes,
		HostKey: host, AuthorizedKeys: writeAuthorizedKeys(t, private, 0o600), Addr: "127.0.0.1:2222",
	})
	inner, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := newBoundedListener(inner, 1)
	paused, resume, handled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	accepted := make(chan net.Conn, 1)
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(resume) })
		_ = listener.Close()
		_ = server.Close()
		// The fallback owns even a connection the broken listener forgot.
		if conn := <-accepted; conn != nil {
			_ = conn.Close()
		}
		waitSSHCompletion(t, handled, time.Now().Add(2*time.Second), "HandleConn cleanup")
	})
	go func() {
		defer close(handled)
		conn, err := listener.Accept()
		accepted <- conn
		if err != nil {
			return
		}
		server.HandleConn(&registrationGapConn{Conn: conn, paused: paused, resume: resume})
	}()
	expected, err := gossh.NewPublicKey(host.Public())
	if err != nil {
		t.Fatal(err)
	}
	config := clientConfig(t, private, gossh.FixedHostKey(expected))
	config.Timeout = 2 * time.Second
	client, err := gossh.Dial("tcp4", inner.Addr().String(), config)
	if err != nil {
		t.Fatal(err)
	}
	disconnected := make(chan struct{})
	t.Cleanup(func() {
		_ = client.Close()
		waitSSHCompletion(t, disconnected, time.Now().Add(2*time.Second), "client Wait cleanup")
	})
	go func() {
		_ = client.Wait()
		close(disconnected)
	}()
	waitSSHCompletion(t, paused, time.Now().Add(2*time.Second), "post-handshake registration gap")
	deadline := time.Now().Add(2 * time.Second)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	waitSSHCompletion(t, disconnected, deadline, "authenticated connection shutdown before registration")
	release.Do(func() { close(resume) })
	waitSSHCompletion(t, handled, deadline, "HandleConn after shutdown")
}

func waitSSHCompletion(t *testing.T, done <-chan struct{}, deadline time.Time, name string) {
	t.Helper()
	select {
	case <-done:
		return
	default:
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Errorf("SSH %s did not join", name)
	}
}

// An accept may already have a socket when closing the underlying listener
// returns. Delay its delivery to exercise the ownership handoff without sleeps.
type delayedAcceptListener struct {
	net.Listener
	conn     net.Conn
	accepted chan struct{}
	resume   chan struct{}
}

func (l *delayedAcceptListener) Accept() (net.Conn, error) {
	close(l.accepted)
	<-l.resume
	return l.conn, nil
}

func TestBoundedListenerClosesAcceptDuringShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		peer, socket := net.Pipe()
		defer peer.Close()   //nolint:errcheck // fixture cleanup
		defer socket.Close() //nolint:errcheck // fixture cleanup
		inner := &delayedAcceptListener{conn: socket, accepted: make(chan struct{}), resume: make(chan struct{})}
		// Close and Addr need only a real listener, not the test socket.
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		inner.Listener = listener
		bounded := newBoundedListener(inner, 1)
		defer bounded.Close() //nolint:errcheck // fixture cleanup
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			conn, err := bounded.Accept()
			checkClosedAccept(t, conn, err)
		}()
		<-inner.accepted
		_ = bounded.Close()
		close(inner.resume)
		<-joined
		if _, err := peer.Write([]byte{1}); err == nil {
			t.Error("accepted socket survived listener shutdown")
		}
	})
}

func checkClosedAccept(t *testing.T, conn net.Conn, err error) {
	t.Helper()
	if conn != nil {
		_ = conn.Close()
		t.Error("Accept returned a connection after listener shutdown")
	}
	if !errors.Is(err, net.ErrClosed) {
		t.Errorf("Accept after shutdown = %v, want net.ErrClosed", err)
	}
}

func TestServeStopsSilentHandshakeWithContext(t *testing.T) {
	endpoint := reserveEndpoint(t, "127.0.0.2")
	ctx, cancel := context.WithCancel(context.Background())
	stopped, disconnected := make(chan struct{}), make(chan struct{})
	lifetime := sshFixtureShutdown{cancel: cancel, served: stopped}
	t.Cleanup(func() { lifetime.cleanup(t) })
	cfg := Config{ScopeMembership: fixtureNoScopes, HostKey: generatePrivateKey(t),
		AuthorizedKeys: writeAuthorizedKeys(t, generatePrivateKey(t), 0o600), Addr: endpoint}
	go func() {
		defer close(stopped)
		if err := Serve(ctx, cfg); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	conn := waitForListener(t, endpoint)
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatalf("read SSH banner before cancellation: %v", err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	lifetime.disconnected = disconnected
	go func() {
		defer close(disconnected)
		_, _ = io.Copy(io.Discard, conn)
	}()
	lifetime.stop()
	waitSSHCompletion(t, stopped, lifetime.deadline, "Serve shutdown")
	waitSSHCompletion(t, disconnected, lifetime.deadline, "silent handshake shutdown")
}

func TestServeCancellationStopsActiveHandler(t *testing.T) {
	endpoint := reserveEndpoint(t, "127.0.0.2")
	private, host := generatePrivateKey(t), generatePrivateKey(t)
	started, finished := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	lifetime := sshFixtureShutdown{cancel: cancel, served: stopped}
	t.Cleanup(func() {
		lifetime.cleanup(t)
		select {
		case <-started:
			waitSSHCompletion(t, finished, lifetime.deadline, "active handler cleanup")
		default:
		}
	})
	cfg := Config{ScopeMembership: fixtureNoScopes, HostKey: host,
		AuthorizedKeys: writeAuthorizedKeys(t, private, 0o600), Addr: endpoint,
		Handler: func(ctx context.Context, _ Session) (int, error) {
			close(started)
			<-ctx.Done()
			close(finished)
			return 0, nil
		}}
	go func() {
		defer close(stopped)
		if err := Serve(ctx, cfg); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	conn := waitForListener(t, endpoint)
	expected, err := gossh.NewPublicKey(host.Public())
	if err != nil {
		t.Fatal(err)
	}
	transport, channels, requests, err := gossh.NewClientConn(conn, endpoint, clientConfig(t, private, gossh.FixedHostKey(expected)))
	if err != nil {
		t.Fatal(err)
	}
	client := gossh.NewClient(transport, channels, requests)
	t.Cleanup(func() { _ = client.Close() })
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	disconnected := make(chan struct{})
	lifetime.disconnected = disconnected
	go func() {
		_ = client.Wait()
		close(disconnected)
	}()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if err := session.Start("ls"); err != nil {
		t.Fatal(err)
	}
	waitSSHCompletion(t, started, time.Now().Add(2*time.Second), "active handler startup")
	lifetime.stop()
	waitSSHCompletion(t, stopped, lifetime.deadline, "Serve shutdown with active handler")
	waitSSHCompletion(t, disconnected, lifetime.deadline, "active handler connection shutdown")
	waitSSHCompletion(t, finished, lifetime.deadline, "active handler shutdown")
}

func pumpBoundedAccepts(listener net.Listener, accepted chan<- net.Conn, stopped chan<- struct{}) {
	defer close(stopped)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}
}
