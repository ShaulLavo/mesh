package apps

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxListenerAddressesRejectExternalBindings(t *testing.T) {
	tests := []struct {
		name, raw string
		ipv6      bool
		allowed   bool
	}{
		{"IPv4 loopback", "0100007F", false, true},
		{"IPv4 wildcard", "00000000", false, false},
		{"IPv4 Tailnet", "02004064", false, false},
		{"IPv4 other loopback", "0200007F", false, false},
		{"IPv6 loopback", "00000000000000000000000001000000", true, true},
		{"IPv6 wildcard", "00000000000000000000000000000000", true, false},
		{"IPv6 mapped loopback", "0000000000000000FFFF00000100007F", true, true},
		{"IPv6 mapped wildcard", "0000000000000000FFFF000000000000", true, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte("  sl  local_address rem_address st\n  0: " + test.raw + ":0BB8 00000000:0000 0A\n")
			addresses, err := parseLinuxListeners(raw, 3000, test.ipv6)
			if err != nil {
				t.Fatal(err)
			}
			err = validateServerAddresses(addresses)
			if (err == nil) != test.allowed {
				t.Fatalf("address=%v allowed=%t error=%v", addresses, test.allowed, err)
			}
		})
	}
}
func TestLinuxListenerTableIncludesEverySelectedPortListener(t *testing.T) {
	raw := []byte("sl local_address rem_address st\n0: 0100007F:0BB8 00000000:0000 0A\n1: 00000000:0BB8 00000000:0000 0A\n2: 00000000:0BB9 00000000:0000 0A\n3: 00000000:0BB8 00000000:0000 01\n")
	addresses, err := parseLinuxListeners(raw, 3000, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(addresses) != 2 || validateServerAddresses(addresses) == nil {
		t.Fatalf("missed external listener: %v", addresses)
	}
	for _, malformed := range []string{"0: malformed", "0: 0100007F:QQQQ 00000000:0000 0A", "0: zzzzzzzz:0BB8 00000000:0000 0A"} {
		if _, err := parseLinuxListeners([]byte(malformed), 3000, false); err == nil {
			t.Fatalf("accepted %q", malformed)
		}
	}
	if _, err := parseLinuxListeners(make([]byte, maximumListenerTableBytes+1), 3000, false); err == nil {
		t.Fatal("unbounded table accepted")
	}
}
func TestDarwinListenerTableRejectsWildcardAndTailnet(t *testing.T) {
	allowed := []byte("Active Internet connections (including servers)\nProto Recv-Q Send-Q Local Address Foreign Address (state)\ntcp4 0 0 127.0.0.1.3000 *.* LISTEN\ntcp6 0 0 ::1.3000 *.* LISTEN\ntcp4 0 0 *.4000 *.* LISTEN\ntcp4 0 0 100.64.0.2.3000 100.64.0.3.5000 ESTABLISHED\n")
	addresses, err := parseDarwinListeners(allowed, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if len(addresses) != 2 || validateServerAddresses(addresses) != nil {
		t.Fatalf("valid loopbacks rejected: %v", addresses)
	}
	for _, row := range []string{"tcp4 0 0 *.3000 *.* LISTEN", "tcp6 0 0 ::.3000 *.* LISTEN", "tcp4 0 0 100.64.0.2.3000 *.* LISTEN"} {
		addresses, err := parseDarwinListeners([]byte(row), 3000)
		if err != nil || validateServerAddresses(addresses) == nil {
			t.Fatalf("unsafe macOS row: %q error=%v", row, err)
		}
	}
	if _, err := parseDarwinListeners([]byte("tcp4 0 0 broken *.* LISTEN"), 3000); err == nil {
		t.Fatal("malformed macOS address accepted")
	}
	if _, err := parseDarwinListeners([]byte("tcp4 0 0 localhost.3000 *.* LISTEN"), 3000); err == nil {
		t.Fatal("nonnumeric macOS address accepted")
	}
}
func TestListenerAddressValidationFailsWithoutEvidence(t *testing.T) {
	if err := validateServerAddresses(nil); err == nil {
		t.Fatal("missing listener accepted")
	}
	if err := validateServerAddresses([]netip.Addr{netip.MustParseAddr("::1"), netip.MustParseAddr("100.64.0.2")}); err == nil {
		t.Fatal("mixed listeners accepted")
	}
	for _, port := range []int{-1, 0, 65536} {
		if err := ProbePortAvailable(port); err == nil {
			t.Fatalf("invalid port %d accepted", port)
		}
	}
}
func TestLiveListenerInspectionAndPortCollision(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port
	if err := checkServerListener(context.Background(), port); err != nil {
		t.Fatal(err)
	}
	if err := ProbePortAvailable(port); err == nil {
		t.Fatal("existing listener accepted by preflight")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ProbePortAvailable(port); err != nil {
		t.Fatal(err)
	}
	if err := checkServerListener(context.Background(), port); err == nil {
		t.Fatal("closed listener accepted")
	}
}
func TestLiveWildcardListenerIsRejected(t *testing.T) {
	listener, err := net.Listen("tcp4", "0.0.0.0:0") //nolint:gosec // Deliberate wildcard fixture verifies rejection.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port
	if err := checkServerListener(context.Background(), port); err == nil {
		t.Fatal("wildcard server accepted")
	}
	if err := ProbePortAvailable(port); err == nil {
		t.Fatal("wildcard preexisting server accepted")
	}
}
func TestDataRootRequiresDirectoryAndFreeSpace(t *testing.T) {
	root := t.TempDir()
	if err := validateDataRoot(filepath.Join(root, "nested", "apps")); err != nil {
		t.Fatal(err)
	}
	if err := validateDataRoot("relative/apps"); err == nil {
		t.Fatal("relative root accepted")
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateDataRoot(file); err == nil {
		t.Fatal("regular file used as workload root")
	}
	if err := validateDataRoot(filepath.Join(file, "apps")); err == nil {
		t.Fatal("nondirectory ancestor accepted")
	}
	if err := checkDataFreeBytes(minimumAppFreeBytes - 1); err == nil {
		t.Fatal("insufficient free space accepted")
	}
	if err := checkDataFreeBytes(minimumAppFreeBytes); err != nil {
		t.Fatal(err)
	}
	if got := statAvailableBytes(^uint64(0), 4096); got != ^uint64(0) {
		t.Fatalf("overflow reported %d", got)
	}
}
func TestListenerInspectionCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := checkServerListener(ctx, 3000); err == nil {
		t.Fatal("canceled inspection accepted")
	}
}
func TestListenerInspectionBuffersAreBounded(t *testing.T) {
	var b listenerTableBuffer
	if _, err := b.Write([]byte("header")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte(strings.Repeat("x", maximumListenerTableBytes))); err == nil {
		t.Fatal("utility output exceeded limit")
	}
	if len(b.bytes) != len("header") {
		t.Fatal("oversized output partially retained")
	}
}
