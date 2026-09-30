package tailnet

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
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

func TestPeerDiagRequestTargetsOnlyClientSocket(t *testing.T) {
	for _, tt := range []struct {
		name, peer, local, source string
		family                    uint8
	}{
		{"IPv4", "127.0.0.1:40000", "127.0.0.1:443", "7f000001000000000000000000000000", unix.AF_INET},
		{"IPv6", "[::1]:40000", "[::1]:443", "00000000000000000000000000000001", unix.AF_INET6},
		{"mapped IPv4", "127.0.0.1:40000", "127.0.0.1:443", "00000000000000000000ffff7f000001", unix.AF_INET6},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := peerDiagID(netip.MustParseAddrPort(tt.peer), netip.MustParseAddrPort(tt.local), tt.family)
			raw, err := marshalPeerDiagRequest(id, tt.family)
			if err != nil {
				t.Fatal(err)
			}
			var header unix.NlMsghdr
			if _, err := binary.Decode(raw, binary.NativeEndian, &header); err != nil {
				t.Fatal(err)
			}
			if len(raw) != unix.NLMSG_HDRLEN+inetDiagReqSize || header.Len != unix.NLMSG_HDRLEN+inetDiagReqSize ||
				header.Type != unix.SOCK_DIAG_BY_FAMILY || header.Flags != unix.NLM_F_REQUEST || header.Seq != peerDiagSequence {
				t.Fatalf("request must be one exact query, never a dump: %+v", header)
			}
			var request inetDiagReqV2
			if _, err := binary.Decode(raw[unix.NLMSG_HDRLEN:], binary.NativeEndian, &request); err != nil {
				t.Fatal(err)
			}
			if request.Family != tt.family || request.Protocol != unix.IPPROTO_TCP || request.States != 1<<tcpEstablished ||
				request.Extensions != 0 || request.Pad != 0 || request.ID.Interface != 0 ||
				request.ID.Cookie != [2]uint32{math.MaxUint32, math.MaxUint32} ||
				hex.EncodeToString(request.ID.Source[:]) != tt.source || hex.EncodeToString(request.ID.Destination[:]) != tt.source ||
				request.ID.SourcePort != [2]byte{0x9c, 0x40} || request.ID.DestinationPort != [2]byte{0x01, 0xbb} {
				t.Fatalf("incorrect reverse-tuple query: %+v", request)
			}
		})
	}
}

func peerDiagReply(t *testing.T, message inetDiagMsg) []byte {
	t.Helper()
	raw, err := binary.Append(nil, binary.NativeEndian, struct {
		Header  unix.NlMsghdr
		Message inetDiagMsg
	}{unix.NlMsghdr{Len: unix.NLMSG_HDRLEN + 72, Type: unix.SOCK_DIAG_BY_FAMILY, Seq: peerDiagSequence}, message})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPeerDiagReplyAuthenticatesClientSocketOnly(t *testing.T) {
	id := peerDiagID(netip.MustParseAddrPort("127.0.0.1:40000"), netip.MustParseAddrPort("127.0.0.1:443"), unix.AF_INET)
	for _, tt := range []struct {
		name   string
		change func(*inetDiagMsg)
		uid    uint32
		bad    bool
	}{
		{name: "client UID", uid: 1000},
		{name: "root forwarder", change: func(m *inetDiagMsg) { m.UID = 0 }},
		{name: "accepted socket", bad: true, change: func(m *inetDiagMsg) { m.ID.SourcePort, m.ID.DestinationPort = m.ID.DestinationPort, m.ID.SourcePort }},
		{name: "wrong client port", bad: true, change: func(m *inetDiagMsg) { m.ID.SourcePort[1]++ }},
		{name: "wrong server port", bad: true, change: func(m *inetDiagMsg) { m.ID.DestinationPort[1]++ }},
		{name: "wrong client address", bad: true, change: func(m *inetDiagMsg) { m.ID.Source[3]++ }},
		{name: "wrong server address", bad: true, change: func(m *inetDiagMsg) { m.ID.Destination[3]++ }},
		{name: "wrong family", bad: true, change: func(m *inetDiagMsg) { m.Family = unix.AF_INET6 }},
		{name: "TIME_WAIT root UID", bad: true, change: func(m *inetDiagMsg) { m.State, m.UID, m.Inode = 6, 0, 0 }},
		{name: "TIME_WAIT with inode", bad: true, change: func(m *inetDiagMsg) { m.State = 6 }},
		{name: "closing socket", bad: true, change: func(m *inetDiagMsg) { m.State = 4 }},
		{name: "no socket inode", bad: true, change: func(m *inetDiagMsg) { m.UID, m.Inode = 0, 0 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			message := inetDiagMsg{Family: unix.AF_INET, State: tcpEstablished, ID: id, UID: 1000, Inode: 101}
			message.ID.Cookie = [2]uint32{17, 23}
			if tt.change != nil {
				tt.change(&message)
			}
			uid, err := parsePeerDiagReply(peerDiagReply(t, message), id, unix.AF_INET)
			if (err != nil) != tt.bad || uid != tt.uid {
				t.Fatalf("diagnostic UID = %d, %v; want %d, error %t", uid, err, tt.uid, tt.bad)
			}
		})
	}
}

func TestPeerDiagReplyAcceptsMappedIPv4Response(t *testing.T) {
	peer, local := netip.MustParseAddrPort("127.0.0.1:40000"), netip.MustParseAddrPort("127.0.0.1:443")
	request := peerDiagID(peer, local, unix.AF_INET)
	message := inetDiagMsg{Family: unix.AF_INET6, State: tcpEstablished, ID: peerDiagID(peer, local, unix.AF_INET6), UID: 1000, Inode: 101}
	uid, err := parsePeerDiagReply(peerDiagReply(t, message), request, unix.AF_INET)
	if err != nil || uid != 1000 {
		t.Fatalf("IPv4 query with mapped IPv6 reply = %d, %v", uid, err)
	}
}

func TestPeerDiagReplyRejectsMalformedMessages(t *testing.T) {
	id := peerDiagID(netip.MustParseAddrPort("127.0.0.1:40000"), netip.MustParseAddrPort("127.0.0.1:443"), unix.AF_INET)
	valid := peerDiagReply(t, inetDiagMsg{Family: unix.AF_INET, State: tcpEstablished, ID: id, UID: 1000, Inode: 101})
	for _, tt := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"empty reply", func([]byte) []byte { return nil }},
		{"short header", func(raw []byte) []byte { return raw[:unix.NLMSG_HDRLEN-1] }},
		{"short socket", func(raw []byte) []byte {
			binary.NativeEndian.PutUint32(raw[:4], unix.NLMSG_HDRLEN+4)
			return raw[:unix.NLMSG_HDRLEN+4]
		}},
		{"oversized length", func(raw []byte) []byte { binary.NativeEndian.PutUint32(raw[:4], math.MaxUint32); return raw }},
		{"short length", func(raw []byte) []byte { binary.NativeEndian.PutUint32(raw[:4], 0); return raw }},
		{"wrong sequence", func(raw []byte) []byte { binary.NativeEndian.PutUint32(raw[8:12], peerDiagSequence+1); return raw }},
		{"multipart dump", func(raw []byte) []byte { binary.NativeEndian.PutUint16(raw[6:8], unix.NLM_F_MULTI); return raw }},
		{"done without socket", func(raw []byte) []byte { binary.NativeEndian.PutUint16(raw[4:6], unix.NLMSG_DONE); return raw }},
		{"short error", func(raw []byte) []byte {
			binary.NativeEndian.PutUint16(raw[4:6], unix.NLMSG_ERROR)
			binary.NativeEndian.PutUint32(raw[:4], unix.NLMSG_HDRLEN+4)
			return raw[:unix.NLMSG_HDRLEN+4]
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if uid, err := parsePeerDiagReply(tt.change(append([]byte(nil), valid...)), id, unix.AF_INET); err == nil || uid != 0 {
				t.Fatalf("malformed reply authenticated UID %d, error %v", uid, err)
			}
		})
	}
}

func TestPeerDiagReplyKernelErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		code uint32
		err  error
	}{
		{"missing peer", ^uint32(unix.ENOENT) + 1, unix.ENOENT},
		{"permission failure", ^uint32(unix.EPERM) + 1, unix.EPERM},
		{"ack without peer", 0, nil},
		{"invalid positive errno", 2, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := binary.Append(nil, binary.NativeEndian, struct {
				Header  unix.NlMsghdr
				Code    uint32
				Request unix.NlMsghdr
			}{Header: unix.NlMsghdr{Len: unix.NLMSG_HDRLEN*2 + 4, Type: unix.NLMSG_ERROR, Seq: peerDiagSequence}, Code: tt.code})
			if err != nil {
				t.Fatal(err)
			}
			uid, err := parsePeerDiagReply(raw, inetDiagSockID{}, unix.AF_INET)
			if uid != 0 || err == nil || tt.err != nil && !errors.Is(err, tt.err) {
				t.Fatalf("kernel error authenticated UID %d, error %v; want rejection, %v", uid, err, tt.err)
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
