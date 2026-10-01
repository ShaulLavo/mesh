package edge

import (
	"net"
	"net/http"
	"net/netip"
	"os"

	"github.com/shaul/mesh/internal/sockdiag"
)

func trustedLoopbackForwarder(r *http.Request) bool {
	return loopbackForwarderUID(r, sockdiag.PeerUID)
}

func loopbackForwarderUID(r *http.Request, lookup func(netip.AddrPort, netip.AddrPort) (uint32, error)) bool {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || !peer.Addr().IsLoopback() {
		return false
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	if !ok {
		return false
	}
	uid, err := lookup(peer, local.AddrPort())
	meshUID := os.Getuid()
	return err == nil && (uid == 0 || meshUID >= 0 && uint64(uid) == uint64(meshUID))
}
