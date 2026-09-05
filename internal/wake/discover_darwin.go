package wake

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

func defaultRoute(ctx context.Context) (route, error) {
	contents, err := runCommand(ctx, "/sbin/route", "-n", "get", "default")
	if err != nil {
		return route{}, err
	}
	return parseDarwinRoute(contents)
}

func parseDarwinRoute(contents []byte) (route, error) {
	var found route
	for _, line := range strings.Split(string(contents), "\n") {
		name, value, _ := strings.Cut(line, ":")
		switch strings.TrimSpace(name) {
		case "gateway":
			found.gateway, _ = netip.ParseAddr(strings.TrimSpace(value))
		case "interface":
			found.device = strings.TrimSpace(value)
		}
	}
	if found.device == "" || !usableIPv4(found.gateway) {
		return route{}, ErrNoLAN
	}
	return found, nil
}

func requireWired(ctx context.Context, device string) error {
	contents, err := runCommand(ctx, "/usr/sbin/networksetup", "-listallhardwareports")
	if err != nil {
		return err
	}
	return parseDarwinWired(contents, device)
}

func parseDarwinWired(contents []byte, device string) error {
	port := ""
	for _, line := range strings.Split(string(contents), "\n") {
		name, value, _ := strings.Cut(line, ":")
		if strings.TrimSpace(name) == "Hardware Port" {
			port = strings.TrimSpace(value)
		}
		if strings.TrimSpace(name) != "Device" || strings.TrimSpace(value) != device {
			continue
		}
		if strings.Contains(strings.ToLower(port), "wi-fi") || strings.Contains(strings.ToLower(port), "airport") {
			return fmt.Errorf("%w: target uses Wi-Fi; configure a wired default-route interface", ErrUnsupportedNIC)
		}
		return nil
	}
	return errors.New("cannot establish whether wake target interface is wired")
}

func gatewayMAC(ctx context.Context, route route) (string, error) {
	contents, err := runCommand(ctx, "/usr/sbin/arp", "-n", route.gateway.String())
	if err != nil {
		return "", err
	}
	return parseDarwinARP(contents, route)
}

func ping(ctx context.Context, address string) (bool, error) {
	return pingResult(ctx, 2, "/sbin/ping", "-n", "-c", "1", "-t", "1", "-W", "1000", address)
}
