//go:build mesh_integration

package daemon

import "net/netip"

func init() {
	tailscaleIPv4Prefix = netip.MustParsePrefix("127.0.0.0/8")
}
