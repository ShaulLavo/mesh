package wake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
)

func defaultRoute(ctx context.Context) (route, error) {
	contents, err := runCommand(ctx, "ip", "-j", "-4", "route", "show", "default")
	if err != nil {
		return route{}, err
	}
	return parseLinuxRoute(contents)
}

func parseLinuxRoute(contents []byte) (route, error) {
	var routes []struct {
		Gateway string `json:"gateway"`
		Device  string `json:"dev"`
		Metric  int    `json:"metric"`
	}
	if err := json.Unmarshal(contents, &routes); err != nil {
		return route{}, fmt.Errorf("parse default routes: %w", err)
	}
	var chosen route
	metric := int(^uint(0) >> 1)
	for _, candidate := range routes {
		gateway, err := netip.ParseAddr(candidate.Gateway)
		if err != nil || !usableIPv4(gateway) || candidate.Device == "" || candidate.Metric >= metric {
			continue
		}
		chosen = route{device: candidate.Device, gateway: gateway}
		metric = candidate.Metric
	}
	if chosen.device == "" {
		return route{}, ErrNoLAN
	}
	return chosen, nil
}

func requireWired(_ context.Context, device string) error {
	_, err := os.Stat(filepath.Join("/sys/class/net", device, "wireless"))
	if err == nil {
		return fmt.Errorf("%w: target uses Wi-Fi; configure a wired default-route interface", ErrUnsupportedNIC)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect wake interface: %w", err)
	}
	return nil
}

func gatewayMAC(ctx context.Context, route route) (string, error) {
	contents, err := runCommand(ctx, "ip", "-j", "-4", "neigh", "show", "to", route.gateway.String(), "dev", route.device)
	if err != nil {
		return "", err
	}
	return parseLinuxNeighbor(contents, route.gateway)
}

func parseLinuxNeighbor(contents []byte, gateway netip.Addr) (string, error) {
	var neighbors []struct {
		Destination string   `json:"dst"`
		MAC         string   `json:"lladdr"`
		State       []string `json:"state"`
	}
	if err := json.Unmarshal(contents, &neighbors); err != nil {
		return "", fmt.Errorf("parse gateway neighbor: %w", err)
	}
	for _, neighbor := range neighbors {
		if neighbor.Destination != gateway.String() || neighbor.MAC == "" {
			continue
		}
		return normalizeMAC(neighbor.MAC)
	}
	return "", errors.New("default gateway has no Ethernet neighbor entry")
}

func ping(ctx context.Context, address string) (bool, error) {
	return pingResult(ctx, 1, "ping", "-n", "-c", "1", "-W", "1", address)
}
