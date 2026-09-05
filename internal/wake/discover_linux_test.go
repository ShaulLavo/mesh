package wake

import (
	"errors"
	"net/netip"
	"testing"
)

func TestLinuxRouteAndNeighborFixtures(t *testing.T) {
	route, err := parseLinuxRoute([]byte(`[{"gateway":"192.168.1.1","dev":"wlan0","metric":600},{"gateway":"10.0.0.1","dev":"eth0","metric":100}]`))
	if err != nil {
		t.Fatal(err)
	}
	if route.device != "eth0" || route.gateway.String() != "10.0.0.1" {
		t.Fatalf("route = %+v", route)
	}
	mac, err := parseLinuxNeighbor([]byte(`[{"dst":"10.0.0.2","lladdr":"02:22:33:44:55:66"},{"dst":"10.0.0.1","lladdr":"02:aa:bb:cc:dd:ee","state":["STALE"]}]`), route.gateway)
	if err != nil || mac != "02:aa:bb:cc:dd:ee" {
		t.Fatalf("neighbor = %s, %v", mac, err)
	}
	if _, err := parseLinuxRoute([]byte(`[{"dev":"tun0"}]`)); !errors.Is(err, ErrNoLAN) {
		t.Fatalf("route without LAN accepted: %v", err)
	}
	if _, err := parseLinuxNeighbor([]byte(`[{"dst":"10.0.0.1","state":["FAILED"]}]`), netip.MustParseAddr("10.0.0.1")); err == nil {
		t.Fatal("failed neighbor accepted")
	}
}
