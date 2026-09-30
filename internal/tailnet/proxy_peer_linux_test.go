package tailnet

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestProxyPeerUIDFromProc(t *testing.T) {
	for _, tt := range []struct {
		name, network, address string
	}{
		{"IPv4", "tcp4", "127.0.0.1:0"},
		{"IPv6", "tcp6", "[::1]:0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			listener, err := net.Listen(tt.network, tt.address)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			address := listener.Addr().String()
			client, err := net.Dial("tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
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

func TestParsePeerUIDAuthenticatesClientSocketOnly(t *testing.T) {
	peer := netip.MustParseAddrPort("127.0.0.1:40000")
	local := netip.MustParseAddrPort("127.0.0.1:443")
	row := func(client, server, state, uid, inode string) string {
		return fmt.Sprintf("0: %s %s %s 00000000:00000000 00:00000000 00000000 %s 0 %s\n", client, server, state, uid, inode)
	}
	const client = "0100007F:9C40"
	const server = "0100007F:01BB"
	for _, tt := range []struct {
		name  string
		raw   string
		ipv6  bool
		uid   uint32
		found bool
		bad   bool
	}{
		{name: "client UID, not accepted UID", raw: row(server, client, "01", "0", "100") + row(client, server, "01", "1000", "101"), uid: 1000, found: true},
		{name: "root forwarder", raw: row(client, server, "01", "0", "101"), found: true},
		{name: "accepted socket only", raw: row(server, client, "01", "0", "100")},
		{name: "wrong client port", raw: row("0100007F:9C41", server, "01", "0", "101")},
		{name: "wrong server port", raw: row(client, "0100007F:01BC", "01", "0", "101")},
		{name: "TIME_WAIT root UID", raw: row(client, server, "06", "0", "0")},
		{name: "closing socket", raw: row(client, server, "04", "0", "101")},
		{name: "no socket inode", raw: row(client, server, "01", "0", "0")},
		{name: "mapped IPv4", ipv6: true, raw: row("0000000000000000FFFF00000100007F:9C40", "0000000000000000FFFF00000100007F:01BB", "01", "1000", "101"), uid: 1000, found: true},
		{name: "malformed row", raw: "0: bad", bad: true},
		{name: "malformed IP", raw: row("garbage:9C40", server, "01", "0", "101"), bad: true},
		{name: "malformed port", raw: row("0100007F:no", server, "01", "0", "101"), bad: true},
		{name: "malformed UID", raw: row(client, server, "01", "no", "101"), bad: true},
		{name: "UID overflow", raw: row(client, server, "01", "4294967296", "101"), bad: true},
		{name: "table limit", raw: strings.Repeat(" ", maximumPeerTableBytes+1), bad: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			uid, found, err := parsePeerUID([]byte(tt.raw), peer, local, tt.ipv6)
			if uid != tt.uid || found != tt.found || (err != nil) != tt.bad {
				t.Fatalf("lookup = %d, %t, %v; want %d, %t, error %t", uid, found, err, tt.uid, tt.found, tt.bad)
			}
		})
	}
}

func TestProxyForwarderUIDs(t *testing.T) {
	uids, err := ProxyForwarderUIDs()
	if err != nil || len(uids) != 2 || uids[0] != 0 || uids[1] != proxyTestUID(t) {
		t.Fatalf("forwarder allow-list = %v, %v", uids, err)
	}
}
