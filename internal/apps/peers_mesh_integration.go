//go:build mesh_integration

package apps

import "net/netip"

func init() { appTailnetIPv4 = netip.MustParsePrefix("127.0.0.0/8") }
