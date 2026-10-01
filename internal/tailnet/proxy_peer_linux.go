package tailnet

import (
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/shaul/mesh/internal/sockdiag"
)

type systemPeerUIDLookup struct{}

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
	uid, err := sockdiag.PeerUID(peer, local)
	if err != nil {
		return 0, fmt.Errorf("inspect forwarder socket UID: %w", err)
	}
	return uid, nil
}
