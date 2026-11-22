package sockdiag

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"time"

	"golang.org/x/sys/unix"
)

// PeerInfo identifies the live established peer socket's kernel owner UID and inode.
type PeerInfo struct {
	UID   uint32
	Inode uint32
}

// PeerUID identifies the live client socket, not the accepted server socket.
func PeerUID(peer, local netip.AddrPort) (uint32, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := Peer(ctx, peer, local)
	if err != nil {
		return 0, err
	}
	return info.UID, nil
}

// Peer identifies the live established peer socket's UID and inode.
func Peer(ctx context.Context, peer, local netip.AddrPort) (PeerInfo, error) {
	peer = netip.AddrPortFrom(peer.Addr().Unmap(), peer.Port())
	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	if !peer.IsValid() || !local.IsValid() || peer.Addr().Is4() != local.Addr().Is4() {
		return PeerInfo{}, errors.New("peer and local socket addresses must have the same valid family")
	}
	family := uint8(unix.AF_INET6)
	if peer.Addr().Is4() {
		family = unix.AF_INET
	}
	id := peerDiagID(peer, local, family)
	request, err := marshalPeerDiagRequest(id, family)
	if err != nil {
		return PeerInfo{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var info PeerInfo
	err = exchange(ctx, request, func(raw []byte) (bool, error) {
		var err error
		info, err = parsePeerDiagReply(raw, id, family)
		return true, err
	})
	if err != nil {
		return PeerInfo{}, err
	}
	return info, nil
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
	return marshalDiagRequest(inetDiagReqV2{Family: family, Protocol: unix.IPPROTO_TCP, States: 1 << tcpEstablished, ID: id}, false)
}

func parsePeerDiagReply(raw []byte, id inetDiagSockID, family uint8) (PeerInfo, error) {
	header, data, err := diagMessage(raw)
	if err != nil {
		return PeerInfo{}, err
	}
	if uint64(header.Len) != uint64(len(raw)) || header.Flags&unix.NLM_F_MULTI != 0 {
		return PeerInfo{}, errors.New("invalid peer socket diagnostic header")
	}
	if header.Type == unix.NLMSG_ERROR {
		return PeerInfo{}, diagError(data)
	}
	if header.Type != unix.SOCK_DIAG_BY_FAMILY {
		return PeerInfo{}, errors.New("unexpected peer socket diagnostic message")
	}
	var socket inetDiagMsg
	if _, err := binary.Decode(data, binary.NativeEndian, &socket); err != nil {
		return PeerInfo{}, fmt.Errorf("decode peer socket diagnostics: %w", err)
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
		return PeerInfo{}, errors.New("peer socket diagnostic tuple does not match")
	}
	// TIME_WAIT diagnostics may name UID zero but have no owning socket.
	if socket.State != tcpEstablished || socket.Inode == 0 {
		return PeerInfo{}, errors.New("live established peer socket owner not found")
	}
	return PeerInfo{UID: socket.UID, Inode: socket.Inode}, nil
}
