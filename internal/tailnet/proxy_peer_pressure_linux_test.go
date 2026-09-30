package tailnet

import (
	"io"
	"net"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestProxyPeerUIDAndStartupWithBusyTCPTable(t *testing.T) {
	// A developer's host may run a live edge. Only disposable CI hosts may
	// run the pressure fixture without a private network namespace.
	if os.Getenv("MESH_PROXY_PRESSURE_ISOLATED") != "1" && os.Getenv("CI") != "true" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command("unshare", "--user", "--map-root-user", "--net", "sh", "-c",
			`ip link set lo up && exec "$@"`, "mesh-proxy-pressure", executable,
			"-test.run=^TestProxyPeerUIDAndStartupWithBusyTCPTable$", "-test.v")
		command.Env = append(os.Environ(), "MESH_PROXY_PRESSURE_ISOLATED=1")
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
			// Do not leave the pressure fixture in TIME_WAIT after the test.
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
	table, err := os.Open("/proc/net/tcp")
	if err != nil {
		t.Fatal(err)
	}
	bytes, readErr := io.Copy(io.Discard, table)
	_ = table.Close()
	if readErr != nil || bytes <= formerTableLimit {
		t.Fatalf("pressure fixture has %d table bytes, error %v; want more than %d", bytes, readErr, formerTableLimit)
	}
	t.Logf("held %d loopback pairs; /proc/net/tcp has %d bytes", pairs, bytes)
	t.Run("lookup", func(t *testing.T) {
		uid, err := (systemPeerUIDLookup{}).PeerUID(connections[len(connections)-1])
		if err != nil || uid != proxyTestUID(t) {
			t.Fatalf("busy-host peer UID = %d, %v; want %d", uid, err, proxyTestUID(t))
		}
	})
	t.Run("startup", func(t *testing.T) {
		uids, err := ProxyForwarderUIDs()
		if err != nil || len(uids) != 2 || uids[0] != 0 || uids[1] != proxyTestUID(t) {
			t.Fatalf("busy-host forwarder allow-list = %v, %v", uids, err)
		}
	})
}
