package apps

import (
	"errors"
	"net"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFreePortKeepsReservationUntilCleanup(t *testing.T) {
	var port int
	t.Run("reserved", func(t *testing.T) {
		port = freePort(t)
		requirePortReservation(t, port)
		if err := ProbePortAvailable(port); err != nil {
			t.Fatalf("reservation prevented preflight: %v", err)
		}
		listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Fatalf("reservation prevented worker startup: %v", err)
		}
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		requirePortReservation(t, port)
	})
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, unix.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 2}, Port: port}); err != nil {
		t.Fatalf("cleanup kept port %d reserved: %v", port, err)
	}
}

func requirePortReservation(t *testing.T, port int) {
	t.Helper()
	for _, family := range []int{unix.AF_INET, unix.AF_INET6} {
		fd, err := unix.Socket(family, unix.SOCK_STREAM, unix.IPPROTO_TCP)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = unix.Close(fd) })
		var address unix.Sockaddr = &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 2}, Port: port}
		if family == unix.AF_INET6 {
			address = &unix.SockaddrInet6{Addr: [16]byte{15: 1}, Port: port}
		}
		if err := unix.Bind(fd, address); !errors.Is(err, unix.EADDRINUSE) {
			t.Fatalf("fixture released port %d for family %d: bind error %v", port, family, err)
		}
	}
}
