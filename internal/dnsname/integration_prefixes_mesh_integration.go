//go:build mesh_integration

package dnsname

import "net/netip"

func init() {
	tailnetIPv4Prefix = netip.MustParsePrefix("127.0.0.0/8")
}
