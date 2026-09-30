package serve

import (
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// CanonicalHost keeps listener identity checks and public route selection on
// the same authority: DNS case, root dots, and forwarding ports are not identity.
// Malformed authorities and scoped IPv6 literals have no canonical Host.
func CanonicalHost(authority string) (string, bool) {
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
		if strings.HasPrefix(authority, "[") {
			address, err := netip.ParseAddr(host)
			return address.String(), err == nil && address.Is6() && address.Zone() == ""
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
