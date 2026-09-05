package wake

import "testing"

func TestDarwinNetworkFixtures(t *testing.T) {
	route, err := parseDarwinRoute([]byte("   route to: default\n    gateway: 192.168.1.1\n  interface: en0\n"))
	if err != nil {
		t.Fatal(err)
	}
	mac, err := parseDarwinARP([]byte("? (192.168.1.1) at 2:aa:bb:cc:dd:e on en0 ifscope [ethernet]"), route)
	if err != nil || mac != "02:aa:bb:cc:dd:0e" {
		t.Fatalf("ARP = %q, %v", mac, err)
	}
	ports := []byte("Hardware Port: Wi-Fi\nDevice: en0\nEthernet Address: 02:11:22:33:44:55\n\nHardware Port: USB Ethernet\nDevice: en4\n")
	if err := parseDarwinWired(ports, "en0"); err == nil {
		t.Fatal("Wi-Fi accepted for target")
	}
	if err := parseDarwinWired(ports, "en4"); err != nil {
		t.Fatal(err)
	}
}
