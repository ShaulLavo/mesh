package apps

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"slices"
	"syscall"
	"testing"

	"github.com/shaul/mesh/internal/sockdiag"

	"golang.org/x/sys/unix"
)

func TestListenerAddressesRejectExternalBindings(t *testing.T) {
	for _, tt := range []struct {
		address string
		allowed bool
	}{
		{"127.0.0.1", true},
		{"0.0.0.0", false},
		{"100.64.0.2", false},
		{"127.0.0.2", false},
		{"::1", true},
		{"::", false},
		{"::ffff:127.0.0.1", true},
		{"::ffff:0.0.0.0", false},
	} {
		t.Run(tt.address, func(t *testing.T) {
			_, err := verifyListeners(3000, ownSockets([]netip.Addr{netip.MustParseAddr(tt.address)}))
			if (err == nil) != tt.allowed {
				t.Fatalf("address %s allowed %t, error %v", tt.address, tt.allowed, err)
			}
		})
	}
}

func TestListenerAttributionRequiresOwnerAndHeldInode(t *testing.T) {
	listeners := []sockdiag.Listener{
		{Address: netip.MustParseAddrPort("127.0.0.1:3000"), UID: 1000, Inode: 11},
		{Address: netip.MustParseAddrPort("[::1]:3000"), UID: 1001, Inode: 12},
		{Address: netip.MustParseAddrPort("127.0.0.1:3000"), UID: 1000, Inode: 13},
		{Address: netip.MustParseAddrPort("0.0.0.0:5173"), UID: 1000, Inode: 14},
		{Address: netip.MustParseAddrPort("0.0.0.0:8080"), UID: 1000, Inode: 15},
		{Address: netip.MustParseAddrPort("0.0.0.0:9090"), UID: 1001, Inode: 16},
	}
	held := map[uint32]bool{11: true, 12: true, 14: true, 16: true}
	got := attributeListeners(listeners, 3000, 1000, held)
	want := []listenerSocket{
		{Address: netip.MustParseAddrPort("127.0.0.1:3000"), Own: true},
		{Address: netip.MustParseAddrPort("[::1]:3000")},
		{Address: netip.MustParseAddrPort("127.0.0.1:3000")},
		{Address: netip.MustParseAddrPort("0.0.0.0:5173"), Own: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("attributed = %+v\nwant %+v", got, want)
	}
}

func TestHeldSocketsFindsAListenersInode(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	address := listener.Addr().(*net.TCPAddr).AddrPort()
	found, err := sockdiag.TCPListeners(context.Background(), int(address.Port()))
	if err != nil || len(found) != 1 {
		t.Fatalf("listeners on %v = %v, %v", address, found, err)
	}
	held, err := heldSockets([]int{os.Getpid(), 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if !held[found[0].Inode] {
		t.Fatalf("socket inode %d of %v not among the test process's descriptors", found[0].Inode, address)
	}
}

// samePortListeners listens on each IPv4 address at one shared port. Binding
// one address and then the next leaves a window in which another process can
// take the port on the second address. A wildcard reservation instead has the
// kernel pick a port free on every address, and SO_REUSEPORT lets only this
// test's sockets join it until all of them listen.
func samePortListeners(t *testing.T, addresses ...[4]byte) int {
	t.Helper()
	reservation := reusePortSocket(t)
	if err := unix.Bind(reservation, &unix.SockaddrInet4{}); err != nil {
		t.Fatal(err)
	}
	local, err := unix.Getsockname(reservation)
	if err != nil {
		t.Fatal(err)
	}
	port := local.(*unix.SockaddrInet4).Port
	for _, address := range addresses {
		fd := reusePortSocket(t)
		if err := unix.Bind(fd, &unix.SockaddrInet4{Port: port, Addr: address}); err != nil {
			t.Fatalf("bind %v:%d: %v", address, port, err)
		}
		if err := unix.Listen(fd, 1); err != nil {
			t.Fatal(err)
		}
	}
	return port
}

func reusePortSocket(t *testing.T) int {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		t.Fatal(err)
	}
	return fd
}

func TestLiveListenerInspectionIncludesEverySelectedPortBinding(t *testing.T) {
	port := samePortListeners(t, [4]byte{127, 0, 0, 1}, [4]byte{127, 0, 0, 2})
	addresses, err := serverListeners(context.Background(), port)
	if err != nil || len(addresses) != 2 {
		t.Fatalf("missed selected-port binding: %v, %v", addresses, err)
	}
	if _, err := checkServerListener(context.Background(), port, thisProcess); err == nil {
		t.Fatalf("unsafe selected-port binding %v accepted", addresses)
	}
}

// TestLiveIPv6ListenerInspection decodes and attributes the IPv6 listener it
// holds. IPv6 loopback does not reserve the port number on IPv4 addresses, so
// another socket, such as another test's 127.0.0.2 listener, may share it; the
// test holds one such neighbour itself and examines only its own socket's
// inode. The rule for every listener on a port belongs to the safety tests.
func TestLiveIPv6ListenerInspection(t *testing.T) {
	for _, address := range []netip.Addr{netip.IPv6Loopback(), netip.MustParseAddr("::ffff:127.0.0.1")} {
		t.Run(address.String(), func(t *testing.T) {
			fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = unix.Close(fd) })
			if err := unix.Bind(fd, &unix.SockaddrInet6{Addr: address.As16()}); err != nil {
				t.Fatal(err)
			}
			if err := unix.Listen(fd, 1); err != nil {
				t.Fatal(err)
			}
			local, err := unix.Getsockname(fd)
			if err != nil {
				t.Fatal(err)
			}
			port := local.(*unix.SockaddrInet6).Port
			// EADDRINUSE means another process already shares the port there.
			neighbour, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2), Port: port})
			if err != nil && !errors.Is(err, syscall.EADDRINUSE) {
				t.Fatal(err)
			}
			if err == nil {
				t.Cleanup(func() { _ = neighbour.Close() })
			}
			var stat unix.Stat_t
			if err := unix.Fstat(fd, &stat); err != nil {
				t.Fatal(err)
			}
			listeners, err := sockdiag.TCPListeners(context.Background(), port)
			if err != nil {
				t.Fatal(err)
			}
			var own []sockdiag.Listener
			for _, listener := range listeners {
				if uint64(listener.Inode) == stat.Ino {
					own = append(own, listener)
				}
			}
			if len(own) != 1 || own[0].Address.Addr() != address || int(own[0].Address.Port()) != port {
				t.Fatalf("listener with inode %d among %+v; want one at %v port %d", stat.Ino, listeners, address, port)
			}
			held, err := heldSockets([]int{os.Getpid()})
			if err != nil {
				t.Fatal(err)
			}
			uid := uint32(os.Geteuid()) //nolint:gosec // an effective UID is a non-negative 32-bit value
			upstream, err := verifyListeners(port, attributeListeners(own, port, uid, held))
			if err != nil || upstream.Addr() != address.Unmap() || int(upstream.Port()) != port {
				t.Fatalf("upstream for %v = %v, %v", address, upstream, err)
			}
		})
	}
}
