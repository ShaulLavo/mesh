//go:build linux

package apps

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/shaul/mesh/internal/sockdiag"

	"golang.org/x/sys/unix"
)

func availableDataBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Bsize <= 0 {
		return 0, errors.New("app: invalid filesystem block size")
	}
	return statAvailableBytes(stat.Bavail, uint64(stat.Bsize)), nil
}
func validateDataMount(root, parent string) error {
	requested := requiredDataMount(root)
	actual := requiredDataMount(parent)
	if requested == "" && actual == "" {
		return nil
	}
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return errors.New("app: cannot verify data SSD mount")
	}
	if requested != "" && !hasDataMount(string(mounts), requested) {
		return fmt.Errorf("app: required data SSD %s is not mounted", requested)
	}
	if actual != "" && !hasDataMount(string(mounts), actual) {
		return fmt.Errorf("app: required data SSD %s is not mounted", actual)
	}
	if requested != "" && actual != requested {
		return errors.New("app: workload directory resolves outside its configured data SSD")
	}
	return nil
}
func requiredDataMount(path string) string {
	for _, mount := range []string{"/work", "/data"} {
		if path == mount || strings.HasPrefix(path, mount+"/") {
			return mount
		}
	}
	return ""
}
func hasDataMount(contents, mount string) bool {
	for _, line := range strings.Split(contents, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || !strings.Contains(line, " - ") {
			continue
		}
		if unescapeMountPath(fields[4]) == mount {
			return true
		}
	}
	return false
}
func unescapeMountPath(path string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(path)
}
func serverListeners(ctx context.Context, port int) ([]netip.Addr, error) {
	if err := validServerPort(port); err != nil {
		return nil, err
	}
	listeners, err := sockdiag.TCPListeners(ctx, port)
	if err != nil {
		return nil, fmt.Errorf("app: inspect TCP listeners on port %d: %w", port, err)
	}
	addresses := make([]netip.Addr, 0, len(listeners))
	for _, listener := range listeners {
		addresses = append(addresses, listener.Address.Addr())
	}
	return addresses, nil
}

// appListeners returns the listeners on port, and every listener the app's
// session holds on other ports, each attributed by the kernel's owner UID and
// by whether a session process has its inode open.
func appListeners(ctx context.Context, port int, processes func() ([]int, error)) ([]listenerSocket, error) {
	listeners, err := sockdiag.AllTCPListeners(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: inspect TCP listeners: %w", err)
	}
	session, err := processes()
	if err != nil {
		return nil, fmt.Errorf("app: list server processes: %w", err)
	}
	held, err := heldSockets(session)
	if err != nil {
		return nil, err
	}
	uid := uint32(os.Geteuid()) //nolint:gosec // an effective UID is a non-negative 32-bit value
	return attributeListeners(listeners, port, uid, held), nil
}

func attributeListeners(listeners []sockdiag.Listener, port int, uid uint32, held map[uint32]bool) []listenerSocket {
	var sockets []listenerSocket
	for _, listener := range listeners {
		own := listener.UID == uid && held[listener.Inode]
		if own || int(listener.Address.Port()) == port {
			sockets = append(sockets, listenerSocket{Address: listener.Address, Own: own, Inode: listener.Inode})
		}
	}
	return sockets
}

// heldSockets returns the socket inodes the processes have open. A process
// that exited meanwhile holds nothing; one whose descriptors cannot be read
// would leave the answer incomplete, so it is an error.
func heldSockets(processes []int) (map[uint32]bool, error) {
	return heldSocketsFrom(processes, readDirNames, os.Readlink)
}

func heldSocketsFrom(processes []int, names func(string) ([]string, error), readlink func(string) (string, error)) (map[uint32]bool, error) {
	held := map[uint32]bool{}
	for _, pid := range processes {
		if err := holdProcessSockets(pid, held, names, readlink); err != nil {
			return nil, err
		}
	}
	return held, nil
}

func holdProcessSockets(pid int, held map[uint32]bool, names func(string) ([]string, error), readlink func(string) (string, error)) error {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := names(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return descriptorInspectionError(fmt.Errorf("app: read open files of server process %d: %w", pid, err))
	}
	for _, name := range entries {
		inode, ok, err := socketInode(dir+"/"+name, readlink)
		if err != nil {
			return err
		}
		if ok {
			held[inode] = true
		}
	}
	return nil
}

func descriptorInspectionError(err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w: %w", errIncompleteListenerObservation, err)
	}
	return err
}

// socketInode reads one descriptor link. A descriptor closed since the
// listing is simply not held.
func socketInode(link string, readlink func(string) (string, error)) (uint32, bool, error) {
	target, err := readlink(link)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, descriptorInspectionError(fmt.Errorf("app: read server descriptor %s: %w", link, err))
	}
	inode, ok := strings.CutPrefix(target, "socket:[")
	if !ok {
		return 0, false, nil
	}
	value, err := strconv.ParseUint(strings.TrimSuffix(inode, "]"), 10, 32)
	return uint32(value), err == nil, nil
}

func readDirNames(dir string) ([]string, error) {
	file, err := os.Open(dir) //nolint:gosec // a /proc descriptor directory of a session process
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	defer file.Close() //nolint:errcheck // read-only
	names, err := file.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	return names, nil
}
