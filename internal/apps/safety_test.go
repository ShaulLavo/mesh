package apps

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// thisProcess makes the test process the app's whole session.
func thisProcess() ([]int, error) { return []int{os.Getpid()}, nil }

// ownSockets treats addresses as the app's own listeners on port 3000.
func ownSockets(addresses []netip.Addr) []listenerSocket {
	sockets := make([]listenerSocket, 0, len(addresses))
	for _, address := range addresses {
		sockets = append(sockets, listenerSocket{Address: netip.AddrPortFrom(address, 3000), Own: true})
	}
	return sockets
}

func TestDarwinListenerTableRejectsWildcardAndTailnet(t *testing.T) {
	allowed := []byte("Active Internet connections (including servers)\nProto Recv-Q Send-Q Local Address Foreign Address (state)\ntcp4 0 0 127.0.0.1.3000 *.* LISTEN\ntcp6 0 0 ::1.3000 *.* LISTEN\ntcp4 0 0 *.4000 *.* LISTEN\ntcp4 0 0 100.64.0.2.3000 100.64.0.3.5000 ESTABLISHED\n")
	addresses, err := parseDarwinListeners(allowed, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if upstream, err := verifyListeners(3000, ownSockets(addresses)); len(addresses) != 2 || err != nil || upstream != netip.MustParseAddrPort("127.0.0.1:3000") {
		t.Fatalf("valid loopbacks rejected: %v", addresses)
	}
	for _, row := range []string{"tcp4 0 0 *.3000 *.* LISTEN", "tcp6 0 0 ::.3000 *.* LISTEN", "tcp4 0 0 100.64.0.2.3000 *.* LISTEN"} {
		addresses, err := parseDarwinListeners([]byte(row), 3000)
		if _, verifyErr := verifyListeners(3000, ownSockets(addresses)); err != nil || verifyErr == nil {
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
	if _, err := verifyListeners(3000, nil); !errors.Is(err, errNoListener) {
		t.Fatalf("missing listener = %v; want errNoListener", err)
	}
	if _, err := verifyListeners(3000, ownSockets([]netip.Addr{netip.MustParseAddr("::1"), netip.MustParseAddr("100.64.0.2")})); err == nil {
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
	if err := checkServerListenerErr(context.Background(), port); err != nil {
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
	if err := checkServerListenerErr(context.Background(), port); err == nil {
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
	if err := checkServerListenerErr(context.Background(), port); err == nil {
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
	if err := checkServerListenerErr(ctx, 3000); err == nil {
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

func checkServerListenerErr(ctx context.Context, port int) error {
	_, err := checkServerListener(ctx, port, thisProcess)
	return err
}

func TestVerifiedListenerChoosesOneAddress(t *testing.T) {
	for _, tt := range []struct {
		name      string
		addresses []string
		want      string
	}{
		{"IPv4", []string{"127.0.0.1"}, "127.0.0.1:3000"},
		{"IPv6", []string{"::1"}, "[::1]:3000"},
		{"IPv4 wins over IPv6", []string{"::1", "127.0.0.1"}, "127.0.0.1:3000"},
		{"mapped IPv4 dials IPv4", []string{"::ffff:127.0.0.1"}, "127.0.0.1:3000"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var addresses []netip.Addr
			for _, address := range tt.addresses {
				addresses = append(addresses, netip.MustParseAddr(address))
			}
			upstream, err := verifyListeners(3000, ownSockets(addresses))
			if err != nil || upstream != netip.MustParseAddrPort(tt.want) {
				t.Fatalf("upstream = %v, %v; want %s", upstream, err, tt.want)
			}
		})
	}
}

func TestVerifiedListenerRejectsForeignAndExposedSockets(t *testing.T) {
	own := func(address string) listenerSocket {
		return listenerSocket{Address: netip.MustParseAddrPort(address), Own: true}
	}
	foreign := func(address string) listenerSocket { return listenerSocket{Address: netip.MustParseAddrPort(address)} }
	for _, tt := range []struct {
		name    string
		sockets []listenerSocket
		fault   string
	}{
		{"foreign loopback on the port", []listenerSocket{foreign("127.0.0.1:3000")}, "outside the app"},
		{"foreign beside the app's own", []listenerSocket{own("127.0.0.1:3000"), foreign("[::1]:3000")}, "outside the app"},
		{"foreign wildcard on the port", []listenerSocket{own("127.0.0.1:3000"), foreign("0.0.0.0:3000")}, "outside the app"},
		{"own wildcard on the port", []listenerSocket{own("0.0.0.0:3000")}, "beyond loopback"},
		{"own wildcard on another port", []listenerSocket{own("127.0.0.1:3000"), own("[::]:5173")}, "beyond loopback"},
		{"own tailnet address on another port", []listenerSocket{own("127.0.0.1:3000"), own("100.64.0.2:5173")}, "beyond loopback"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := verifyListeners(3000, tt.sockets)
			var fault listenerFault
			if !errors.As(err, &fault) || !strings.Contains(string(fault), tt.fault) {
				t.Fatalf("verify = %v; want a fault naming %q", err, tt.fault)
			}
		})
	}
	upstream, err := verifyListeners(3000, []listenerSocket{own("127.0.0.1:3000"), own("127.0.0.1:24678")})
	if err != nil || upstream != netip.MustParseAddrPort("127.0.0.1:3000") {
		t.Fatalf("own loopback listener on another port refused: %v, %v", upstream, err)
	}
}
