package wake

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

const sopassMax = 6

// linkFile is lexically earlier than the 99-default.link every distribution
// ships, because systemd applies only the first .link file that matches a
// device. linkDir is a variable so tests need neither root nor a real /etc.
var linkDir = "/etc/systemd/network"

const linkFile = "90-mesh-wake.link"

const linkHeader = `# Managed by Mesh. Written by "mesh wake allow", removed by "mesh wake deny".
# Keeps wake on magic packet armed across reboots and NIC hotplug.
#
# systemd applies only the first matching .link file, so the [Link] settings
# below are inherited from the file that matched before Mesh took over. Editing
# them by hand is fine; Mesh only ever rewrites this whole file.
`

type ethtoolWolInfo struct {
	Cmd       uint32
	Supported uint32
	Wolopts   uint32
	Sopass    [sopassMax]byte
}

type ethtoolIfreq struct {
	Name [unix.IFNAMSIZ]byte
	Data *ethtoolWolInfo
}

// Inspect reports what the NIC behind mac will do on a magic packet. It needs
// root: the kernel gates ETHTOOL_GWOL behind CAP_NET_ADMIN.
func Inspect(ctx context.Context, mac string) (ArmState, error) {
	device, err := deviceForMAC(mac)
	if err != nil {
		return ArmState{}, err
	}
	state := ArmState{Device: device}
	info := ethtoolWolInfo{Cmd: unix.ETHTOOL_GWOL}
	if err := ethtoolWOL(device, &info); err != nil {
		return ArmState{Device: device}, err
	}
	state.Supported = info.Supported&unix.WAKE_MAGIC != 0
	state.Armed = info.Wolopts&unix.WAKE_MAGIC != 0
	state.Firmware = firmwareWakeState(device)
	state.Persisted, err = linkFileArms(mac)
	if err != nil {
		return state, err
	}
	return state, nil
}

// firmwareWakeState reads the ACPI wake source for the NIC's PCI function.
// A board that lists the NIC as disabled never delivers the packet to the
// driver, and no amount of ethtool state changes that.
func firmwareWakeState(device string) string {
	link, err := os.Readlink(filepath.Join("/sys/class/net", device, "device"))
	if err != nil {
		return ""
	}
	contents, err := os.ReadFile("/proc/acpi/wakeup")
	if err != nil {
		return ""
	}
	return parseACPIWakeup(contents, "pci:"+filepath.Base(link))
}

func parseACPIWakeup(contents []byte, slot string) string {
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || fields[len(fields)-1] != slot {
			continue
		}
		// The status column carries a leading "*" when the source is armed.
		if strings.Contains(fields[2], "disabled") {
			return "disabled"
		}
		return "enabled"
	}
	return ""
}

// Arm turns on wake for magic packets and makes it survive a reboot. It is
// idempotent, so callers may run it on every "wake allow".
func Arm(ctx context.Context, mac string) (ArmState, error) {
	state, err := Inspect(ctx, mac)
	if err != nil {
		return state, err
	}
	if !state.Supported {
		return state, fmt.Errorf("%w: %s reports no magic packet support", ErrArmUnsupported, state.Device)
	}
	if !state.Armed {
		if err := setWOL(state.Device, unix.WAKE_MAGIC); err != nil {
			return state, err
		}
	}
	if err := writeLinkFile(ctx, mac, state.Device); err != nil {
		return state, err
	}
	// Re-read rather than trusting the write: a driver may accept ETHTOOL_SWOL
	// and silently keep wake off, and a grant must not outrun the hardware.
	verified, err := Inspect(ctx, mac)
	if err != nil {
		return verified, err
	}
	if !verified.Ready() {
		return verified, fmt.Errorf("%w: %s did not keep wake on magic packet", ErrArmUnsupported, verified.Device)
	}
	return verified, nil
}

// Disarm reverses Arm. A missing link file or an already-disarmed NIC is not an
// error, so "wake deny" is safe to run twice.
func Disarm(ctx context.Context, mac string) (ArmState, error) {
	if err := removeLinkFile(); err != nil {
		return ArmState{}, err
	}
	device, err := deviceForMAC(mac)
	if err != nil {
		// The NIC may be gone entirely. The persisted file is what matters.
		return ArmState{}, nil
	}
	info := ethtoolWolInfo{Cmd: unix.ETHTOOL_GWOL}
	if err := ethtoolWOL(device, &info); err != nil {
		return ArmState{Device: device}, err
	}
	if info.Wolopts&unix.WAKE_MAGIC != 0 {
		if err := setWOL(device, info.Wolopts&^unix.WAKE_MAGIC); err != nil {
			return ArmState{Device: device}, err
		}
	}
	return Inspect(ctx, mac)
}

func setWOL(device string, options uint32) error {
	info := ethtoolWolInfo{Cmd: unix.ETHTOOL_SWOL, Wolopts: options}
	if err := ethtoolWOL(device, &info); err != nil {
		return fmt.Errorf("set wake on LAN for %s: %w", device, err)
	}
	return nil
}

func ethtoolWOL(device string, info *ethtoolWolInfo) error {
	if device == "" || len(device) >= unix.IFNAMSIZ {
		return fmt.Errorf("invalid interface name %q", device)
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open ethtool socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	var request ethtoolIfreq
	copy(request.Name[:], device)
	request.Data = info
	// SIOCETHTOOL takes an ifreq whose union member is a pointer to the command
	// struct. x/sys keeps its ifreqData type unexported, so the layout is built
	// here; request holds a typed *ethtoolWolInfo so the pointee stays reachable.
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.SIOCETHTOOL), uintptr(unsafe.Pointer(&request))) //nolint:gosec // G103: the ifreq layout is fixed by the kernel ABI and the pointee is kept alive below
	runtime.KeepAlive(info)
	if errno == 0 {
		return nil
	}
	switch {
	case errors.Is(errno, unix.EPERM), errors.Is(errno, unix.EACCES):
		return ErrArmPrivilege
	case errors.Is(errno, unix.EOPNOTSUPP), errors.Is(errno, unix.ENOTSUP):
		return fmt.Errorf("%w: %s has no ethtool wake support", ErrArmUnsupported, device)
	default:
		return fmt.Errorf("ethtool ioctl on %s: %w", device, errno)
	}
}

func linkFilePath() string { return filepath.Join(linkDir, linkFile) }

// linkFileArms reports whether the persisted file is Mesh's and still names
// this NIC. A file for a different MAC arms a different machine's hardware.
func linkFileArms(mac string) (bool, error) {
	contents, err := os.ReadFile(linkFilePath())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", linkFilePath(), err)
	}
	var matchesMAC, wakesOnMagic bool
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	for scanner.Scan() {
		key, value, found := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		matchesMAC = matchesMAC || (strings.EqualFold(key, "MACAddress") && strings.EqualFold(value, mac))
		wakesOnMagic = wakesOnMagic || (strings.EqualFold(key, "WakeOnLan") && strings.EqualFold(value, "magic"))
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("read %s: %w", linkFilePath(), err)
	}
	return matchesMAC && wakesOnMagic, nil
}

func writeLinkFile(ctx context.Context, mac, device string) error {
	inherited, err := inheritedLinkSettings(ctx, device)
	if err != nil {
		return err
	}
	contents := renderLinkFile(mac, inherited)
	// 0755 root:root is what every distribution ships for this directory, and
	// it normally already exists; tightening it here would be a surprise.
	if err := os.MkdirAll(linkDir, 0o755); err != nil { //nolint:gosec // G301: standard permissions for /etc/systemd/network
		return fmt.Errorf("create %s: %w", linkDir, err)
	}
	// Root-owned and world-readable: systemd-udevd reads this at boot, and it
	// must never be writable by the user who ran "mesh wake allow".
	if err := writeRootFile(linkFilePath(), contents); err != nil {
		return err
	}
	return reloadUdevRules(ctx)
}

func removeLinkFile() error {
	if err := os.Remove(linkFilePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", linkFilePath(), err)
	}
	return nil
}

func renderLinkFile(mac string, inherited []string) []byte {
	builder := &strings.Builder{}
	builder.WriteString(linkHeader)
	builder.WriteString("\n[Match]\nMACAddress=")
	builder.WriteString(mac)
	builder.WriteString("\n\n[Link]\n")
	for _, setting := range inherited {
		builder.WriteString(setting)
		builder.WriteString("\n")
	}
	builder.WriteString("WakeOnLan=magic\n")
	return []byte(builder.String())
}

// inheritedLinkSettings copies the [Link] settings of whichever file governs
// the device today. Without this, taking over with a narrower match would drop
// the distribution's NamePolicy and rename the interface on the next boot.
func inheritedLinkSettings(ctx context.Context, device string) ([]string, error) {
	path, err := currentLinkFile(ctx, device)
	if err != nil || path == "" || path == linkFilePath() {
		return nil, err
	}
	contents, err := os.ReadFile(path) //nolint:gosec // path comes from udevadm's ID_NET_LINK_FILE for a validated device
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return parseLinkSettings(contents), nil
}

func parseLinkSettings(contents []byte) []string {
	var settings []string
	var inLink bool
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			inLink = strings.EqualFold(line, "[Link]")
			continue
		}
		key, _, found := strings.Cut(line, "=")
		if !inLink || !found {
			continue
		}
		// Mesh owns WakeOnLan; everything else is the distribution's business.
		if strings.EqualFold(strings.TrimSpace(key), "WakeOnLan") {
			continue
		}
		settings = append(settings, line)
	}
	return settings
}

func currentLinkFile(ctx context.Context, device string) (string, error) {
	contents, err := runCommand(ctx, "udevadm", "info", "--query=property", "--path=/sys/class/net/"+device)
	if err != nil {
		// udevadm is absent on some minimal images. Inheriting nothing is worse
		// than not persisting at all, so surface it.
		return "", fmt.Errorf("inspect udev link file for %s: %w", device, err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(contents))
	for scanner.Scan() {
		if value, found := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "ID_NET_LINK_FILE="); found {
			return value, nil
		}
	}
	return "", nil
}

func writeRootFile(path string, contents []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".mesh-wake-*")
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	defer func() { _ = file.Close() }()
	if _, err := file.Write(contents); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Chmod(0o644); err != nil {
		return fmt.Errorf("set permissions on %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("publish %s: %w", path, err)
	}
	return nil
}

// reloadUdevRules makes the new file govern the running system too, so that a
// re-plugged cable does not silently drop back to the old settings.
func reloadUdevRules(ctx context.Context) error {
	if _, err := runCommand(ctx, "udevadm", "control", "--reload"); err != nil {
		return fmt.Errorf("reload udev rules: %w", err)
	}
	return nil
}
