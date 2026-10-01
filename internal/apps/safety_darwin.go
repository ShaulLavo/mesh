//go:build darwin

package apps

import (
	"context"
	"errors"
	"net/netip"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

func availableDataBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return statAvailableBytes(uint64(stat.Bavail), uint64(stat.Bsize)), nil
}
func validateDataMount(root, parent string) error { return nil }
func serverListeners(ctx context.Context, port int) ([]netip.Addr, error) {
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var out listenerTableBuffer
	cmd := exec.CommandContext(bounded, "/usr/sbin/netstat", "-an", "-p", "tcp")
	cmd.Stdout = &out
	// Ignore utility stderr; it can contain unrelated process diagnostics.
	if err := cmd.Run(); err != nil {
		return nil, errors.New("app: cannot verify TCP listeners with macOS netstat")
	}
	return parseDarwinListeners(out.bytes, port)
}

// appListeners counts every listener on the port as the app's own: macOS has no
// way to map a socket to a process short of lsof or libproc, and Mesh uses
// neither. Only addresses are checked there, and listeners on other ports are
// not seen.
func appListeners(ctx context.Context, port int, _ func() ([]int, error)) ([]listenerSocket, error) {
	addresses, err := serverListeners(ctx, port)
	if err != nil {
		return nil, err
	}
	sockets := make([]listenerSocket, 0, len(addresses))
	for _, address := range addresses {
		sockets = append(sockets, listenerSocket{Address: netip.AddrPortFrom(address, uint16(port)), Own: true}) //nolint:gosec // checkServerListener bounds the port first
	}
	return sockets, nil
}
