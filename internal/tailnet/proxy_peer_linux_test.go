package tailnet

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"

	"golang.org/x/sys/unix"
)

func TestProxyPeerUIDFromDiagnostics(t *testing.T) {
	for _, tt := range []struct {
		name, network, address string
	}{
		{"IPv4", "tcp4", "127.0.0.1:0"},
		{"IPv6", "tcp6", "[::1]:0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			listener, client := proxyClient(t, tt.network, tt.address)
			server, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			uid, err := (systemPeerUIDLookup{}).PeerUID(server)
			if err != nil || uid != proxyTestUID(t) {
				t.Fatalf("peer UID = %d, %v; want %d", uid, err, proxyTestUID(t))
			}
			if _, err := io.WriteString(client, "PROXY TCP4 100.64.0.2 127.0.0.1 40000 443\r\npayload"); err != nil {
				t.Fatal(err)
			}
			proxy := NewProxyConn(server, []uint32{uid})
			body := make([]byte, len("payload"))
			if _, err := io.ReadFull(proxy, body); err != nil || string(body) != "payload" || proxy.RemoteAddr().String() != "100.64.0.2:40000" {
				t.Fatalf("authenticated source or payload lost: %s %q %v", proxy.RemoteAddr(), body, err)
			}
		})
	}
}

func TestProxyPeerUIDFromMappedIPv4Socket(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if err := unix.Connect(fd, &unix.SockaddrInet6{
		Port: listener.Addr().(*net.TCPAddr).Port,
		Addr: netip.MustParseAddr("::ffff:127.0.0.1").As16(),
	}); err != nil {
		t.Fatal(err)
	}
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	uid, err := (systemPeerUIDLookup{}).PeerUID(server)
	if err != nil || uid != proxyTestUID(t) {
		t.Fatalf("mapped IPv4 peer UID = %d, %v; want %d", uid, err, proxyTestUID(t))
	}
}

func TestProxyPeerUIDRejectsClosedClient(t *testing.T) {
	listener, client := proxyClient(t, "tcp4", "127.0.0.1:0")
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("client FIN not received: %v", err)
	}
	if uid, err := (systemPeerUIDLookup{}).PeerUID(server); err == nil {
		t.Fatalf("closing client authenticated as UID %d", uid)
	}
}

func TestProxyListenerRejectsRealDisallowedPeerUID(t *testing.T) {
	listener, client := proxyClient(t, "tcp4", "127.0.0.1:0")
	proxyListener := ProxyListener{Listener: listener, AllowedUIDs: []uint32{proxyTestUID(t) + 1}}
	server, err := proxyListener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if _, err := io.WriteString(client, "PROXY TCP4 100.64.0.2 127.0.0.1 40000 443\r\npayload"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Read(make([]byte, 1)); err == nil {
		t.Fatal("real peer UID exclusion did not reject PROXY metadata")
	}
	if got := server.RemoteAddr().String(); got != client.LocalAddr().String() {
		t.Fatalf("rejected peer exposed source %s", got)
	}
}

func TestProxyForwarderUIDs(t *testing.T) {
	uids, err := ProxyForwarderUIDs()
	if err != nil || len(uids) != 2 || uids[0] != 0 || uids[1] != proxyTestUID(t) {
		t.Fatalf("forwarder allow-list = %v, %v", uids, err)
	}
}
