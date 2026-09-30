package tailnet

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

type peerUIDFunc func(net.Conn) (uint32, error)

func (f peerUIDFunc) PeerUID(c net.Conn) (uint32, error) { return f(c) }

func TestProxyListenerAuthenticatesForwarderUID(t *testing.T) {
	uid := uint32(os.Getuid())
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
			if !tt.accept {
				if err == nil || n != 0 {
					t.Fatalf("disallowed forwarder exposed payload %q and forged RemoteAddr %s", body[:n], server.RemoteAddr())
				}
				if got := server.RemoteAddr().String(); got != client.LocalAddr().String() {
					t.Fatalf("disallowed forwarder exposed forged RemoteAddr %s", got)
				}
				_ = client.SetReadDeadline(time.Now().Add(time.Second))
				if _, err := client.Read(make([]byte, 1)); err == nil {
					t.Fatal("rejected peer connection remained open")
				} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
					t.Fatal("rejected peer connection was not closed")
				}
			} else if err != nil || string(body) != "payload" || server.RemoteAddr().String() != "100.64.0.2:40000" {
				t.Fatalf("allowed forwarder lost source or payload: %s %q %v", server.RemoteAddr(), body[:n], err)
			}
			if !lookupCalled && len(tt.allowed) != 0 {
				t.Fatal("forwarder UID lookup was not used")
			}
		})
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
