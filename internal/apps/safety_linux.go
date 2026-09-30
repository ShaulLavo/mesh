//go:build linux

package apps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"

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
	var addresses []netip.Addr
	for _, table := range []struct {
		path string
		ipv6 bool
	}{{"/proc/net/tcp", false}, {"/proc/net/tcp6", true}} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, err := os.Open(table.path)
		if err != nil {
			return nil, errors.New("app: cannot verify kernel TCP listeners")
		}
		raw, readErr := io.ReadAll(io.LimitReader(f, maximumListenerTableBytes+1))
		closeErr := f.Close()
		if readErr != nil || closeErr != nil {
			return nil, errors.New("app: cannot read kernel TCP listeners")
		}
		parsed, err := parseLinuxListeners(raw, port, table.ipv6)
		if err != nil {
			return nil, err
		}
		addresses = append(addresses, parsed...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return addresses, nil
}
