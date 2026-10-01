package apps

import (
	"context"
	"net"
	"net/netip"
	"testing"

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
			err := validateServerAddresses([]netip.Addr{netip.MustParseAddr(tt.address)})
			if (err == nil) != tt.allowed {
				t.Fatalf("address %s allowed %t, error %v", tt.address, tt.allowed, err)
			}
		})
	}
}

func TestLiveListenerInspectionIncludesEverySelectedPortBinding(t *testing.T) {
	first, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	port := first.Addr().(*net.TCPAddr).Port
	second, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	addresses, err := serverListeners(context.Background(), port)
	if err != nil || len(addresses) != 2 || validateServerAddresses(addresses) == nil {
		t.Fatalf("missed unsafe selected-port binding: %v, %v", addresses, err)
	}
}

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
			addresses, err := serverListeners(context.Background(), port)
			if err != nil || len(addresses) != 1 || addresses[0] != address {
				t.Fatalf("IPv6 listeners = %v, %v; want %v", addresses, err, address)
			}
			if err := checkServerListener(context.Background(), port); err != nil {
				t.Fatal(err)
			}
		})
	}
}
