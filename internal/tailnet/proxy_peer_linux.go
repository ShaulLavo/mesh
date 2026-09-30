package tailnet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const (
	peerDiagSequence = 1
	tcpEstablished   = 1
	inetDiagReqSize  = 56
)

type systemPeerUIDLookup struct{}

// Ports and addresses are network-endian inside otherwise native-endian netlink
// structures. Byte arrays preserve that distinction without unsafe casts.
type inetDiagSockID struct {
	SourcePort      [2]byte
	DestinationPort [2]byte
	Source          [16]byte
	Destination     [16]byte
	Interface       uint32
	Cookie          [2]uint32
}

type inetDiagReqV2 struct {
	Family     uint8
	Protocol   uint8
	Extensions uint8
	Pad        uint8
	States     uint32
	ID         inetDiagSockID
}

type inetDiagMsg struct {
	Family       uint8
	State        uint8
	Timer        uint8
	Retransmits  uint8
	ID           inetDiagSockID
	Expires      uint32
	ReceiveQueue uint32
	SendQueue    uint32
	UID          uint32
	Inode        uint32
}

func ProxyForwarderUIDs() ([]uint32, error) {
	return proxyForwarderUIDs(systemPeerUIDLookup{})
}

func proxyForwarderUIDs(lookup peerUIDLookup) ([]uint32, error) {
	uid := int64(os.Getuid())
	if uid < 0 || uid > math.MaxUint32 {
		return nil, fmt.Errorf("tailnet: invalid Mesh UID %d", uid)
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, fmt.Errorf("tailnet: PROXY forwarder authentication unavailable: listen for owned loopback probe: %w", err)
	}
	defer func() { _ = listener.Close() }()
	client, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if err != nil {
		return nil, fmt.Errorf("tailnet: PROXY forwarder authentication unavailable: connect owned loopback probe: %w", err)
	}
	defer func() { _ = client.Close() }()
	server, err := listener.AcceptTCP()
	if err != nil {
		return nil, fmt.Errorf("tailnet: PROXY forwarder authentication unavailable: accept owned loopback probe: %w", err)
	}
	defer func() { _ = server.Close() }()
	// ENOENT also means a missing inet_diag/tcp_diag handler. Only a successful
	// lookup of our live client proves the kernel can authenticate forwarders.
	owner, err := lookup.PeerUID(server)
	if err != nil {
		return nil, fmt.Errorf("tailnet: PROXY forwarder authentication unavailable: inspect owned loopback probe: %w", err)
	}
	if owner != uint32(uid) {
		return nil, fmt.Errorf("tailnet: PROXY forwarder authentication unavailable: owned loopback probe UID %d differs from Mesh UID %d", owner, uid)
	}
	return []uint32{0, uint32(uid)}, nil
}

func (systemPeerUIDLookup) PeerUID(c net.Conn) (uint32, error) {
	peer, err := netip.ParseAddrPort(c.RemoteAddr().String())
	if err != nil {
		return 0, fmt.Errorf("parse peer socket address: %w", err)
	}
	local, err := netip.ParseAddrPort(c.LocalAddr().String())
	if err != nil {
		return 0, fmt.Errorf("parse local socket address: %w", err)
	}
	peer = netip.AddrPortFrom(peer.Addr().Unmap(), peer.Port())
	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	if peer.Addr().Is4() != local.Addr().Is4() {
		return 0, errors.New("peer and local socket address families differ")
	}
	if peer.Addr().Is4() {
		return queryPeerDiag(peer, local, unix.AF_INET)
	}
	return queryPeerDiag(peer, local, unix.AF_INET6)
}

func peerDiagID(peer, local netip.AddrPort, family uint8) inetDiagSockID {
	id := inetDiagSockID{Cookie: [2]uint32{math.MaxUint32, math.MaxUint32}}
	binary.BigEndian.PutUint16(id.SourcePort[:], peer.Port())
	binary.BigEndian.PutUint16(id.DestinationPort[:], local.Port())
	if family == unix.AF_INET {
		source, destination := peer.Addr().As4(), local.Addr().As4()
		copy(id.Source[:], source[:])
		copy(id.Destination[:], destination[:])
	} else {
		id.Source, id.Destination = peer.Addr().As16(), local.Addr().As16()
	}
	return id
}

func marshalPeerDiagRequest(id inetDiagSockID, family uint8) ([]byte, error) {
	request, err := binary.Append(nil, binary.NativeEndian, struct {
		Header  unix.NlMsghdr
		Request inetDiagReqV2
	}{
		Header: unix.NlMsghdr{
			Len: unix.NLMSG_HDRLEN + inetDiagReqSize, Type: unix.SOCK_DIAG_BY_FAMILY,
			Flags: unix.NLM_F_REQUEST, Seq: peerDiagSequence,
		},
		Request: inetDiagReqV2{Family: family, Protocol: unix.IPPROTO_TCP, States: 1 << tcpEstablished, ID: id},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal peer socket diagnostic request: %w", err)
	}
	return request, nil
}

func queryPeerDiag(peer, local netip.AddrPort, family uint8) (uint32, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_SOCK_DIAG)
	if err != nil {
		return 0, fmt.Errorf("open peer socket diagnostics: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 1}); err != nil {
		return 0, fmt.Errorf("bound peer socket diagnostic wait: %w", err)
	}
	// The accepted socket belongs to Mesh. The reverse tuple identifies only
	// the forwarder's socket, whose UID conveys authority.
	id := peerDiagID(peer, local, family)
	request, err := marshalPeerDiagRequest(id, family)
	if err != nil {
		return 0, fmt.Errorf("encode peer socket diagnostic query: %w", err)
	}
	if err := unix.Sendto(fd, request, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return 0, fmt.Errorf("send peer socket diagnostic query: %w", err)
	}
	reply := make([]byte, 4096)
	n, _, flags, sender, err := unix.Recvmsg(fd, reply, nil, 0)
	if err != nil {
		return 0, fmt.Errorf("receive peer socket diagnostics: %w", err)
	}
	kernel, ok := sender.(*unix.SockaddrNetlink)
	if !ok || kernel.Pid != 0 || kernel.Groups != 0 || flags&unix.MSG_TRUNC != 0 {
		return 0, errors.New("invalid peer socket diagnostic sender or truncated reply")
	}
	return parsePeerDiagReply(reply[:n], id, family)
}

func peerDiagError(data []byte) error {
	if len(data) < 4+unix.NLMSG_HDRLEN {
		return errors.New("incomplete peer socket diagnostic error")
	}
	code := binary.NativeEndian.Uint32(data[:4])
	if code <= math.MaxInt32 {
		return errors.New("unexpected peer socket diagnostic acknowledgement")
	}
	return fmt.Errorf("query peer socket diagnostics: %w", unix.Errno(^code+1))
}

func parsePeerDiagReply(raw []byte, id inetDiagSockID, family uint8) (uint32, error) {
	var header unix.NlMsghdr
	if _, err := binary.Decode(raw, binary.NativeEndian, &header); err != nil {
		return 0, fmt.Errorf("decode peer socket diagnostic header: %w", err)
	}
	if uint64(header.Len) != uint64(len(raw)) || header.Len < unix.NLMSG_HDRLEN ||
		header.Seq != peerDiagSequence || header.Flags&unix.NLM_F_MULTI != 0 {
		return 0, errors.New("invalid peer socket diagnostic header")
	}
	data := raw[unix.NLMSG_HDRLEN:]
	if header.Type == unix.NLMSG_ERROR {
		return 0, peerDiagError(data)
	}
	if header.Type != unix.SOCK_DIAG_BY_FAMILY {
		return 0, errors.New("unexpected peer socket diagnostic message")
	}
	var socket inetDiagMsg
	if _, err := binary.Decode(data, binary.NativeEndian, &socket); err != nil {
		return 0, fmt.Errorf("decode peer socket diagnostics: %w", err)
	}
	// AF_INET can find a mapped IPv6 socket, but the kernel replies in the
	// socket's native family. Compare the same tuple in that representation.
	if family == unix.AF_INET && socket.Family == unix.AF_INET6 {
		id.Source = netip.AddrFrom4([4]byte(id.Source[:4])).As16()
		id.Destination = netip.AddrFrom4([4]byte(id.Destination[:4])).As16()
		family = unix.AF_INET6
	}
	if socket.Family != family || socket.ID.SourcePort != id.SourcePort || socket.ID.DestinationPort != id.DestinationPort ||
		socket.ID.Source != id.Source || socket.ID.Destination != id.Destination {
		return 0, errors.New("peer socket diagnostic tuple does not match")
	}
	// TIME_WAIT diagnostics may name UID zero but have no owning socket.
	if socket.State != tcpEstablished || socket.Inode == 0 {
		return 0, errors.New("live established peer socket owner not found")
	}
	return socket.UID, nil
}
