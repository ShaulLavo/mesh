package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/tailnet"
)

var tailscaleIPv4Prefix = netip.MustParsePrefix("100.64.0.0/10")
var tailscaleIPv6Prefix = netip.MustParsePrefix("fd7a:115c:a1e0::/48")

// ErrTailnetAddressesChanged asks the service supervisor to restart the daemon
// so its direct control listeners bind the newly discovered address set.
var ErrTailnetAddressesChanged = errors.New("daemon: Tailscale addresses changed")

func normalizeTailnetAddresses(values []string) ([]string, error) {
	seen := make(map[netip.Addr]struct{}, len(values))
	addresses := make([]netip.Addr, 0, len(values))
	for _, value := range values {
		address, err := netip.ParseAddr(value)
		if err != nil {
			return nil, fmt.Errorf("parse address %q: %w", value, err)
		}
		address = address.Unmap()
		if address.IsUnspecified() || address.IsMulticast() {
			return nil, fmt.Errorf("address %q is not a concrete unicast address", value)
		}
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	slices.SortFunc(addresses, netip.Addr.Compare)
	normalized := make([]string, len(addresses))
	for index, address := range addresses {
		normalized[index] = address.String()
	}
	return normalized, nil
}

func validateTailscaleServeAddresses(addresses []string) error {
	foundIPv4 := false
	for _, value := range addresses {
		address, err := netip.ParseAddr(value)
		if err != nil {
			return fmt.Errorf("daemon: validate Tailscale Serve address %q: %w", value, err)
		}
		address = address.Unmap()
		switch {
		case address.Is4() && tailscaleIPv4Prefix.Contains(address):
			foundIPv4 = true
		case address.Is6() && tailscaleIPv6Prefix.Contains(address):
		default:
			return fmt.Errorf("daemon: Tailscale Serve address %s is outside the Tailscale IPv4 and IPv6 ranges", address)
		}
	}
	if foundIPv4 {
		return nil
	}
	return errors.New("daemon: Tailscale Serve requires a discovered IPv4 address in 100.64.0.0/10")
}

func monitorTailnetAddresses(
	ctx context.Context,
	interval time.Duration,
	discoveryTimeout time.Duration,
	initial []string,
	discover func(context.Context) (tailnet.Peer, error),
	validate func([]string) error,
	report func(error),
) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		discoveryCtx, cancelDiscovery := context.WithTimeout(ctx, discoveryTimeout)
		peer, err := discover(discoveryCtx)
		cancelDiscovery()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			report(fmt.Errorf("daemon: rediscover Tailscale addresses: %w", err))
			continue
		}
		addresses, err := normalizeTailnetAddresses(peer.Addrs)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			report(fmt.Errorf("daemon: normalize rediscovered Tailscale addresses: %w", err))
			continue
		}
		if len(addresses) == 0 {
			report(errors.New("daemon: rediscovered no Tailscale addresses; retaining current control listeners"))
			continue
		}
		if err := validate(addresses); err != nil {
			report(fmt.Errorf("daemon: rediscovered unusable Tailscale addresses; retaining current control listeners: %w", err))
			continue
		}
		if slices.Equal(initial, addresses) {
			continue
		}
		return fmt.Errorf("%w: [%s] -> [%s]", ErrTailnetAddressesChanged, strings.Join(initial, ", "), strings.Join(addresses, ", "))
	}
}
