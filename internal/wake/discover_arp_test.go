package wake

import (
	"net/netip"
	"testing"
)

func TestDarwinARPSelectsGatewayInterface(t *testing.T) {
	selected := route{device: "en4", gateway: netip.MustParseAddr("192.168.1.1")}
	contents := []byte("? (192.168.1.1) at 2:aa:bb:cc:dd:e on en0 ifscope [ethernet]\n" +
		"? (192.168.1.2) at 2:aa:bb:cc:dd:f on en4 ifscope [ethernet]\n" +
		"? (192.168.1.1) at 2:aa:bb:cc:dd:e on en4 ifscope [ethernet]\n")
	mac, err := parseDarwinARP(contents, selected)
	if err != nil || mac != "02:aa:bb:cc:dd:0e" {
		t.Fatalf("gateway on selected interface = %q, %v", mac, err)
	}
}

func TestDarwinARPRejectsMissingGatewayInterface(t *testing.T) {
	selected := route{device: "en4", gateway: netip.MustParseAddr("192.168.1.1")}
	contents := []byte("? (192.168.1.1) at 2:aa:bb:cc:dd:e on en0 ifscope [ethernet]\n" +
		"? (192.168.1.2) at 2:aa:bb:cc:dd:f on en4 ifscope [ethernet]\n")
	if mac, err := parseDarwinARP(contents, selected); err == nil {
		t.Fatalf("unmatched gateway accepted: %q", mac)
	}
}
