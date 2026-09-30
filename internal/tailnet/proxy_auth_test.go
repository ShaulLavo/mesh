package tailnet

import (
	"crypto/tls"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"testing"
	"time"
)

type peerUIDFunc func(net.Conn) (uint32, error)

func (f peerUIDFunc) PeerUID(c net.Conn) (uint32, error) { return f(c) }

func proxyTestUID(t *testing.T) uint32 {
	t.Helper()
	uid := int64(os.Getuid())
	if uid >= 0 && uid <= math.MaxUint32 {
		return uint32(uid)
	}
	t.Fatalf("test process UID %d is outside the kernel UID range", uid)
	return 0
}

func TestProxyListenerAuthenticatesForwarderUID(t *testing.T) {
	uid := proxyTestUID(t)
	for _, tt := range []struct {
		name    string
		allowed []uint32
		err     error
		accept  bool
	}{
		{name: "disallowed", allowed: []uint32{uid + 1}},
		{name: "allowed", allowed: []uint32{uid}, accept: true},
		{name: "no allowed UIDs"},
		{name: "lookup failure", allowed: []uint32{uid}, err: errors.New("socket owner unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			listener, client := proxyClient(t, "tcp4", "127.0.0.1:0")
			lookupCalled := false
			proxyListener := ProxyListener{Listener: listener, AllowedUIDs: tt.allowed, peerUID: peerUIDFunc(func(c net.Conn) (uint32, error) {
				lookupCalled = true
				if c.RemoteAddr().String() != client.LocalAddr().String() {
					t.Errorf("lookup received claimed source instead of socket peer: %s", c.RemoteAddr())
				}
				return uid, tt.err
			})}
			server, err := proxyListener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			if _, err := io.WriteString(client, "PROXY TCP4 100.64.0.2 127.0.0.1 40000 443\r\npayload"); err != nil {
				t.Fatal(err)
			}
			_ = server.SetReadDeadline(time.Now().Add(time.Second))
			body := make([]byte, len("payload"))
			n, err := io.ReadFull(server, body)
			if !lookupCalled && len(tt.allowed) != 0 {
				t.Fatal("forwarder UID lookup was not used")
			}
			if tt.accept {
				if err != nil || string(body) != "payload" || server.RemoteAddr().String() != "100.64.0.2:40000" {
					t.Fatalf("allowed forwarder lost source or payload: %s %q %v", server.RemoteAddr(), body[:n], err)
				}
				return
			}
			if err == nil || n != 0 {
				t.Fatalf("disallowed forwarder exposed payload %q and forged RemoteAddr %s", body[:n], server.RemoteAddr())
			}
			if got := server.RemoteAddr().String(); got != client.LocalAddr().String() {
				t.Fatalf("disallowed forwarder exposed forged RemoteAddr %s", got)
			}
			_ = client.SetReadDeadline(time.Now().Add(time.Second))
			_, readErr := client.Read(make([]byte, 1))
			if readErr == nil {
				t.Fatal("rejected peer connection remained open")
			}
			var timeout net.Error
			if errors.As(readErr, &timeout) && timeout.Timeout() {
				t.Fatal("rejected peer connection was not closed")
			}
		})
	}
}

func TestProxyListenerRejectsBeforeTLS(t *testing.T) {
	listener, client := proxyClient(t, "tcp4", "127.0.0.1:0")
	proxyListener := ProxyListener{Listener: listener, AllowedUIDs: []uint32{42}, peerUID: peerUIDFunc(func(net.Conn) (uint32, error) { return 43, nil })}
	server, err := proxyListener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if _, err := io.WriteString(client, "PROXY TCP4 100.64.0.2 127.0.0.1 40000 443\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = client.SetDeadline(time.Now().Add(time.Second))
	_ = server.SetDeadline(time.Now().Add(time.Second))
	type handshakeResult struct {
		hello bool
		err   error
	}
	done := make(chan handshakeResult, 1)
	go func() {
		hello := false
		tlsServer := tls.Server(server, &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			hello = true
			return nil, errors.New("unauthenticated ClientHello reached TLS")
		}})
		err := tlsServer.Handshake()
		done <- handshakeResult{hello: hello, err: err}
	}()
	tlsClient := tls.Client(client, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "apps.example.test"})
	if err := tlsClient.Handshake(); err == nil {
		t.Fatal("disallowed forwarder completed TLS")
	}
	result := <-done
	if result.err == nil || result.hello {
		t.Fatalf("disallowed forwarder reached TLS: ClientHello=%t, error=%v", result.hello, result.err)
	}
	if got := server.RemoteAddr().String(); got != client.LocalAddr().String() {
		t.Fatalf("TLS rejection exposed source %s", got)
	}
}

func TestProxyListenerRequiresHeaderFromAuthenticatedForwarder(t *testing.T) {
	listener, client := proxyClient(t, "tcp4", "127.0.0.1:0")
	proxyListener := ProxyListener{Listener: listener, AllowedUIDs: []uint32{42}, peerUID: peerUIDFunc(func(net.Conn) (uint32, error) { return 42, nil })}
	server, err := proxyListener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if _, err := io.WriteString(client, "GET / HTTP/1.1\r\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Read(make([]byte, 1)); err == nil {
		t.Fatal("authenticated forwarder bypassed mandatory PROXY header")
	}
	if got := server.RemoteAddr().String(); got != client.LocalAddr().String() {
		t.Fatalf("missing header changed source to %s", got)
	}
}

func proxyClient(t *testing.T, network, address string) (net.Listener, net.Conn) {
	t.Helper()
	listener, err := net.Listen(network, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	client, err := net.Dial(network, listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return listener, client
}
