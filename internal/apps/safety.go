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

// listenerSocket is one listening TCP socket as far as the platform can
// attribute it. Own means the daemon's user owns it and a process in the app's
// worker session holds it. Inode is the kernel's socket identity, zero where
// the platform cannot report it.
type listenerSocket struct {
	Address netip.AddrPort
	Own     bool
	Inode   uint32
}

// errNoListener means the app has not bound its port yet; readiness waits for it.
var errNoListener = errors.New("app: no server listener found on the configured port")

// listenerFault is why an app's listeners must not be proxied to. Its text is
// what the owner sees.
type listenerFault string

func (f listenerFault) Error() string { return string(f) }

// checkServerListener inspects the app's listeners and returns the one address
// the proxy may dial. processes names the app's worker session; it is asked
// only after the listeners are listed, so a process the app started meanwhile
// cannot leave its socket looking foreign. Mesh does not isolate the app's
// network: this check reports what the app bound, it does not prevent it.
func checkServerListener(ctx context.Context, port int, processes func() ([]int, error)) (listenerSocket, error) {
	if err := validServerPort(port); err != nil {
		return listenerSocket{}, err
	}
	sockets, err := appListeners(ctx, port, processes)
	if err != nil {
		return listenerSocket{}, err
	}
	return verifyListeners(port, sockets)
}

// verifyListeners requires every listener on the app's port to be the app's own
// and on loopback, and every listener the app holds on any port to be on
// loopback. IPv4 wins over IPv6 so a dual-stack server is always dialled the
// same way. The result carries the dialled socket's inode, so a listener later
// rebound at the same address is told apart from the one verified.
func verifyListeners(port int, sockets []listenerSocket) (listenerSocket, error) {
	var upstream listenerSocket
	for _, socket := range sockets {
		address := socket.Address.Addr().Unmap()
		if socket.Own && !loopbackListener(address) {
			return listenerSocket{}, listenerFault(fmt.Sprintf("app listens on %s beyond loopback; bind only 127.0.0.1 or ::1", socket.Address))
		}
		if int(socket.Address.Port()) != port {
			continue
		}
		if !socket.Own {
			return listenerSocket{}, listenerFault(fmt.Sprintf("port %d is held by a process outside the app", port))
		}
		if address.Is4() || !upstream.Address.IsValid() {
			upstream = listenerSocket{Address: netip.AddrPortFrom(address, socket.Address.Port()), Own: true, Inode: socket.Inode}
		}
	}
	if !upstream.Address.IsValid() {
		return listenerSocket{}, errNoListener
	}
	return upstream, nil
}

func loopbackListener(address netip.Addr) bool {
	return address == netip.MustParseAddr("127.0.0.1") || address == netip.IPv6Loopback()
}
func validServerPort(port int) error {
	if port < 1 || port > 65535 {
		return errors.New("app: server port must be from 1 to 65535")
	}
	return nil
}
