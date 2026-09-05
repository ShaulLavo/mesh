package wake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type route struct {
	device  string
	gateway netip.Addr
}

// Discover records the default-route wired interface while the target is awake.
func Discover(ctx context.Context) (NIC, error) { return discoverNIC(ctx, true) }

func discoverNIC(ctx context.Context, wiredOnly bool) (NIC, error) {
	discoveryCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	route, err := defaultRoute(discoveryCtx)
	if err != nil {
		return NIC{}, fmt.Errorf("discover wake route: %w", err)
	}
	if wiredOnly {
		if err := requireWired(discoveryCtx, route.device); err != nil {
			return NIC{}, err
		}
	}
	iface, err := net.InterfaceByName(route.device)
	if err != nil {
		return NIC{}, fmt.Errorf("discover wake interface: %w", err)
	}
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagBroadcast == 0 || iface.Flags&net.FlagPointToPoint != 0 {
		return NIC{}, ErrNoLAN
	}
	address, prefix, err := interfaceAddress(iface, route.gateway)
	if err != nil {
		return NIC{}, err
	}
	mac, err := gatewayMAC(discoveryCtx, route)
	if err != nil {
		_, _ = ping(discoveryCtx, route.gateway.String())
		mac, err = gatewayMAC(discoveryCtx, route)
	}
	if err != nil {
		return NIC{}, fmt.Errorf("discover wake gateway MAC: %w", err)
	}
	nic := NIC{MAC: iface.HardwareAddr.String(), Address: address.String(), Prefix: prefix.String(), GatewayMAC: mac}
	if err := validateNIC(nic); err != nil {
		return NIC{}, err
	}
	return nic, nil
}

func interfaceAddress(iface *net.Interface, gateway netip.Addr) (netip.Addr, netip.Prefix, error) {
	addresses, err := iface.Addrs()
	if err != nil {
		return netip.Addr{}, netip.Prefix{}, err
	}
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address.String())
		if err != nil || !usableIPv4(prefix.Addr()) || !prefix.Contains(gateway) {
			continue
		}
		return prefix.Addr(), prefix.Masked(), nil
	}
	return netip.Addr{}, netip.Prefix{}, ErrNoLAN
}

func runCommand(ctx context.Context, command string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, command, args...) //nolint:gosec // platform callers select fixed network utilities; network values remain separate arguments
	output := &commandOutput{}
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.WaitDelay = 100 * time.Millisecond
	err := cmd.Run()
	if commandCtx.Err() != nil {
		return nil, commandCtx.Err()
	}
	if output.overflow {
		return nil, errors.New("wake discovery command output exceeds 64 KiB")
	}
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", command, err)
	}
	return output.buffer.Bytes(), nil
}

type commandOutput struct {
	buffer   bytes.Buffer
	overflow bool
}

func (b *commandOutput) Write(value []byte) (int, error) {
	remaining := max(0, (64<<10)-b.buffer.Len())
	b.overflow = b.overflow || len(value) > remaining
	_, _ = b.buffer.Write(value[:min(len(value), remaining)])
	return len(value), nil
}

func normalizeMAC(value string) (string, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 6 {
		return "", errors.New("invalid gateway MAC")
	}
	mac := make(net.HardwareAddr, 6)
	for index, part := range parts {
		value, err := strconv.ParseUint(part, 16, 8)
		if err != nil {
			return "", errors.New("invalid gateway MAC")
		}
		mac[index] = byte(value)
	}
	canonical := mac.String()
	_, err := parseMAC(canonical)
	return canonical, err
}

func pingResult(ctx context.Context, noReplyExit int, command string, args ...string) (bool, error) {
	_, err := runCommand(ctx, command, args...)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == noReplyExit {
		return false, nil
	}
	return false, err
}
