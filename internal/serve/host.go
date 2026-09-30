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
	if address, err := netip.ParseAddr(authority); err == nil {
		return address.String(), address.Zone() == ""
	}
	host, ok := authorityHost(authority)
	if !ok {
		return "", false
	}
	if address, err := netip.ParseAddr(host); err == nil {
		return address.String(), address.Zone() == ""
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

func authorityHost(authority string) (string, bool) {
	if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
		return bracketedIPv6Host(authority[1 : len(authority)-1])
	}
	if !strings.Contains(authority, ":") {
		return authority, true
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		return "", false
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return "", false
	}
	if strings.HasPrefix(authority, "[") {
		return bracketedIPv6Host(host)
	}
	return host, true
}

func bracketedIPv6Host(host string) (string, bool) {
	address, err := netip.ParseAddr(host)
	return address.String(), err == nil && address.Is6() && address.Zone() == ""
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
