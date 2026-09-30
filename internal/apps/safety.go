package apps

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const minimumAppFreeBytes uint64 = 128 << 20

// validateDataRoot checks the destination before uploads or owner-authorized setup.
// Setup scripts retain the host's execution privileges; this is a storage check.
func validateDataRoot(root string) error {
	if !filepath.IsAbs(root) {
		return errors.New("app: workload root must be an absolute directory")
	}
	parent, err := existingDirectory(root)
	if err != nil {
		return fmt.Errorf("app: inspect workload directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("app: resolve workload directory: %w", err)
	}
	if err := validateDataMount(filepath.Clean(root), resolved); err != nil {
		return err
	}
	free, err := availableDataBytes(resolved)
	if err != nil {
		return fmt.Errorf("app: inspect workload free space: %w", err)
	}
	return checkDataFreeBytes(free)
}
func existingDirectory(path string) (string, error) {
	for {
		info, err := os.Stat(path)
		if err == nil {
			if !info.IsDir() {
				return "", errors.New("workload path is not a directory")
			}
			return path, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		path = parent
	}
}
func checkDataFreeBytes(free uint64) error {
	if free < minimumAppFreeBytes {
		return errors.New("app: workload filesystem needs at least 128 MiB free before uploading or setup")
	}
	return nil
}

// ProbePortAvailable is a preflight check, not a port reservation. Validate the
// actual listeners after startup before making the app ready.
func ProbePortAvailable(port int) error {
	if err := validServerPort(port); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addresses, err := serverListeners(ctx, port)
	if err != nil {
		return err
	}
	if len(addresses) != 0 {
		return errors.New("app: server port already has a listener; choose a free port")
	}
	ipv4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return errors.New("app: server port is unavailable on IPv4 loopback")
	}
	defer ipv4.Close() //nolint:errcheck // the bind result is authoritative
	ipv6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	if errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EPROTONOSUPPORT) || errors.Is(err, syscall.EADDRNOTAVAIL) {
		return nil
	}
	if err != nil {
		return errors.New("app: server port is unavailable on IPv6 loopback")
	}
	return ipv6.Close()
}

// checkServerListener rejects wildcard, Tailnet, and LAN bindings. It does not
// establish OS process or network isolation for owner-authorized app commands.
func checkServerListener(ctx context.Context, port int) error {
	if err := validServerPort(port); err != nil {
		return err
	}
	addresses, err := serverListeners(ctx, port)
	if err != nil {
		return err
	}
	return validateServerAddresses(addresses)
}
func validServerPort(port int) error {
	if port < 1 || port > 65535 {
		return errors.New("app: server port must be from 1 to 65535")
	}
	return nil
}
func validateServerAddresses(addresses []netip.Addr) error {
	if len(addresses) == 0 {
		return errors.New("app: no server listener found on the configured port")
	}
	ipv4 := netip.MustParseAddr("127.0.0.1")
	ipv6 := netip.IPv6Loopback()
	for _, address := range addresses {
		if address.Unmap() != ipv4 && address != ipv6 {
			return errors.New("app: server must bind only 127.0.0.1 or ::1; configure its host explicitly")
		}
	}
	return nil
}
