package daemon

import (
	"net"
	"net/http"
	"net/netip"
	"slices"

	meshserve "github.com/shaul/mesh/internal/serve"
)

type httpHostPolicy struct {
	tailnetAddrs              []netip.Addr
	tailnetNames              []string
	privateName               func() string
	trustPublicEdgeForwarding func(netip.Addr) bool
}

func privateHTTPHandler(handler http.Handler, policy httpHostPolicy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !policy.accepts(r) {
			http.Error(w, "Misdirected Request", http.StatusMisdirectedRequest)
			return
		}
		if handler == nil {
			http.NotFound(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

func (p httpHostPolicy) accepts(request *http.Request) bool {
	host, ok := meshserve.CanonicalHost(request.Host)
	if !ok {
		return false
	}
	if address, err := netip.ParseAddr(host); err == nil {
		return address.Unmap().IsLoopback() || slices.Contains(p.tailnetAddrs, address.Unmap())
	}
	if host == "localhost" {
		return true
	}
	for _, name := range p.tailnetNames {
		if canonical, ok := meshserve.CanonicalHost(name); ok && host == canonical {
			return true
		}
	}
	if p.privateName != nil {
		if name, ok := meshserve.CanonicalHost(p.privateName()); ok && host == name {
			return true
		}
	}
	if p.trustPublicEdgeForwarding == nil || meshserve.ValidatePublicName(host) != nil {
		return false
	}
	peer, err := netip.ParseAddrPort(request.RemoteAddr)
	return err == nil && p.trustPublicEdgeForwarding(peer.Addr().Unmap())
}

func boundHTTPAddresses(listeners []net.Listener) []netip.Addr {
	addresses := make([]netip.Addr, 0, len(listeners))
	for _, listener := range listeners {
		addresses = append(addresses, listener.Addr().(*net.TCPAddr).AddrPort().Addr().Unmap())
	}
	return addresses
}
