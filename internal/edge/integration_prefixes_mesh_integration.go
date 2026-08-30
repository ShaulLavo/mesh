//go:build mesh_integration

package edge

import "net/netip"

func init() {
	tailscaleIPv4 = netip.MustParsePrefix("127.0.0.0/8")
}
