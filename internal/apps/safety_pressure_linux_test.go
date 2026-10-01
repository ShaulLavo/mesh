package apps

import (
	"context"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAppListenerInspectionWithBusyTCPTable(t *testing.T) {
	// Only disposable CI hosts may fill the TCP table without a private namespace.
	if os.Getenv("MESH_APP_PRESSURE_ISOLATED") != "1" && os.Getenv("CI") != "true" {
		executable, err := os.Open("/proc/self/exe")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = executable.Close() }()
		command := exec.Command("unshare", "--user", "--map-root-user", "--net", "sh", "-c",
			`ip link set lo up && exec /proc/self/fd/3 -test.run=^TestAppListenerInspectionWithBusyTCPTable$ -test.v`)
		command.ExtraFiles = []*os.File{executable}
		command.Env = append(os.Environ(), "MESH_APP_PRESSURE_ISOLATED=1")
		output, err := command.CombinedOutput()
		t.Logf("isolated pressure test:\n%s", output)
		if err != nil {
			t.Fatalf("run pressure test in a private network namespace: %v", err)
		}
		return
	}
	const pairs = 15000
	const formerTableLimit = 4 << 20
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	if limit.Cur < 2*pairs+1024 {
		original := limit
		limit.Cur = limit.Max
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
				t.Error(err)
			}
		})
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	connections := make([]*net.TCPConn, 0, pairs*2)
	t.Cleanup(func() {
		for _, connection := range connections {
			// Avoid leaving the established fixture in TIME_WAIT on CI hosts.
			_ = connection.SetLinger(0)
			_ = connection.Close()
		}
	})
	for range pairs {
		client, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, client)
		server, err := listener.AcceptTCP()
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, server)
	}
	clientPort := connections[len(connections)-2].LocalAddr().(*net.TCPAddr).Port
	timeWaitPort := connections[0].LocalAddr().(*net.TCPAddr).Port
	for i := 0; i < 512*2; i += 2 {
		client, server := connections[i], connections[i+1]
		if err := client.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, server); err != nil {
			t.Fatal(err)
		}
		if err := server.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, client); err != nil {
			t.Fatal(err)
		}
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
	}
	table, err := os.Open("/proc/net/tcp")
	if err != nil {
		t.Fatal(err)
	}
	bytes, readErr := io.Copy(io.Discard, table)
	_ = table.Close()
	if readErr != nil || bytes <= formerTableLimit {
		t.Fatalf("pressure fixture has %d table bytes, error %v; want more than %d", bytes, readErr, formerTableLimit)
	}
	t.Logf("held %d loopback pairs, closed 512 into TIME_WAIT; /proc/net/tcp has %d bytes", pairs, bytes)
	port := listener.Addr().(*net.TCPAddr).Port
	t.Run("readiness", func(t *testing.T) {
		if err := checkServerListenerErr(context.Background(), port); err != nil {
			t.Fatalf("busy-host listener readiness: %v", err)
		}
	})
	t.Run("listener addresses", func(t *testing.T) {
		addresses, err := serverListeners(context.Background(), port)
		if err != nil || len(addresses) != 1 || addresses[0] != netip.MustParseAddr("127.0.0.1") {
			t.Fatalf("busy-host listeners = %v, %v; want only 127.0.0.1", addresses, err)
		}
	})
	for _, selected := range []struct {
		name string
		port int
	}{{"ignore established sockets", clientPort}, {"ignore TIME_WAIT sockets", timeWaitPort}} {
		t.Run(selected.name, func(t *testing.T) {
			addresses, err := serverListeners(context.Background(), selected.port)
			if err != nil || len(addresses) != 0 {
				t.Fatalf("busy-host non-listeners = %v, %v; want none", addresses, err)
			}
		})
	}
	t.Run("preflight", func(t *testing.T) {
		free, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		freePort := free.Addr().(*net.TCPAddr).Port
		if err := free.Close(); err != nil {
			t.Fatal(err)
		}
		if err := ProbePortAvailable(freePort); err != nil {
			t.Fatalf("busy-host port preflight: %v", err)
		}
	})
}
