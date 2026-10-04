package sshd

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	charmssh "charm.land/ssh"
	"charm.land/wish/v2/testsession"
	gossh "golang.org/x/crypto/ssh"
)

type unsignedQuerySigner struct {
	key     gossh.PublicKey
	queried chan struct{}
	release chan struct{}
}

func (s unsignedQuerySigner) PublicKey() gossh.PublicKey { return s.key }
func (s unsignedQuerySigner) Sign(io.Reader, []byte) (*gossh.Signature, error) {
	close(s.queried)
	<-s.release
	return nil, errors.New("fixture has no private signing material")
}

type observedGraceConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *observedGraceConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })
	if err != nil {
		return fmt.Errorf("close observed fixture connection: %w", err)
	}
	return nil
}

func TestUnsignedApprovedQueryKeepsLoginGrace(t *testing.T) {
	private := generatePrivateKey(t)
	server := shortGraceServer(t, private)
	closed := make(chan struct{})
	callback := server.ConnCallback
	server.ConnCallback = func(ctx charmssh.Context, conn net.Conn) net.Conn {
		return callback(ctx, &observedGraceConn{Conn: conn, closed: closed})
	}
	address := testsession.Listen(t, server)
	public, err := gossh.NewPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	signer := unsignedQuerySigner{key: public, queried: make(chan struct{}), release: make(chan struct{})}
	config := clientConfig(t, private, nil)
	config.Auth = []gossh.AuthMethod{gossh.PublicKeys(signer)}
	done := make(chan error, 1)
	t.Cleanup(func() {
		close(signer.release)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("query client did not finish")
		}
	})
	go func() {
		client, err := gossh.Dial("tcp", address, config)
		if client != nil {
			_ = client.Close()
		}
		done <- err
	}()
	select {
	case <-signer.queried:
	case <-time.After(5 * time.Second):
		t.Fatal("approved public-key query was not accepted")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("unsigned approved query cleared login grace")
	}
}

func shortGraceServer(t *testing.T, private ed25519.PrivateKey) *charmssh.Server {
	t.Helper()
	server, err := newServer(normalizedConfig{
		loginGrace:     time.Second,
		hostKey:        generatePrivateKey(t),
		authorizedKeys: writeAuthorizedKeys(t, private, 0o600),
		addr:           mustAddrPort(t, "127.0.0.1:2222"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestSignedAuthenticationOutlivesLoginGrace(t *testing.T) {
	private := generatePrivateKey(t)
	server := shortGraceServer(t, private)
	address := testsession.Listen(t, server)
	client, err := gossh.Dial("tcp", address, clientConfig(t, private, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	time.Sleep(1200 * time.Millisecond)
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	if err := session.Run(""); err != nil {
		t.Fatal(err)
	}
}

func TestSilentTransportKeepsLoginGrace(t *testing.T) {
	server := shortGraceServer(t, generatePrivateKey(t))
	closed := make(chan struct{})
	callback := server.ConnCallback
	server.ConnCallback = func(ctx charmssh.Context, conn net.Conn) net.Conn {
		return callback(ctx, &observedGraceConn{Conn: conn, closed: closed})
	}
	address := testsession.Listen(t, server)
	client, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("silent unauthenticated transport outlived login grace")
	}
}
