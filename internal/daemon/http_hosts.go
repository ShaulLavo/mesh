package daemon

import (
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

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
	host, ok := privateRequestHost(request.Host)
	if !ok {
		return false
	}
	if address, err := netip.ParseAddr(host); err == nil {
		return slices.Contains(p.tailnetAddrs, address.Unmap())
	}
	for _, name := range p.tailnetNames {
		if host == strings.ToLower(strings.TrimSuffix(name, ".")) {
			return true
		}
	}
	if p.privateName != nil {
		name := p.privateName()
		if name != "" {
			if host == name {
				return true
			}
			if label, found := strings.CutSuffix(host, "."+name); found && !strings.Contains(label, ".") {
				return true
			}
		}
	}
	if p.trustPublicEdgeForwarding == nil || meshserve.ValidatePublicName(host) != nil {
		return false
	}
	peer, err := netip.ParseAddrPort(request.RemoteAddr)
	return err == nil && p.trustPublicEdgeForwarding(peer.Addr().Unmap())
}

func privateRequestHost(authority string) (string, bool) {
	host := authority
	if address, err := netip.ParseAddr(host); err == nil {
		return address.String(), address.Zone() == ""
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		address, err := netip.ParseAddr(host[1 : len(host)-1])
		return address.String(), err == nil && address.Is6() && address.Zone() == ""
	}
	if strings.Contains(host, ":") {
		var service string
		var err error
		host, service, err = net.SplitHostPort(authority)
		if err != nil {
			return "", false
		}
		number, err := strconv.ParseUint(service, 10, 16)
		if err != nil || number == 0 {
			return "", false
		}
		if address, err := netip.ParseAddr(host); err == nil {
			return address.String(), address.Zone() == ""
		}
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if len(host) > 253 {
		return "", false
	}
	for _, label := range strings.Split(host, ".") {
		if !validHostLabel(label) {
			return "", false
		}
	}
	return host, true
}

func validHostLabel(label string) bool {
	if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, character := range label {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-') {
			return false
		}
	}
	return true
}
