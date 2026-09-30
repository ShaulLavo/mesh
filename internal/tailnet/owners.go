package tailnet

import (
	"context"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// OwnerResolver maps authenticated Tailnet device addresses to configured Mesh
// origins belonging to the same Tailscale user. Tagged devices confer no rights.
func OwnerResolver(origins map[string]string) func(context.Context, netip.Addr) ([]string, error) {
	return NewClient(execRunner{}).OwnerResolver(origins)
}

func (c *Client) OwnerResolver(origins map[string]string) func(context.Context, netip.Addr) ([]string, error) {
	var mu sync.Mutex
	var expires time.Time
	var status rawStatus
	return func(ctx context.Context, ip netip.Addr) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		if !time.Now().Before(expires) {
			fresh, err := c.status(ctx)
			if err != nil {
				return nil, err
			}
			status = fresh
			expires = time.Now().Add(5 * time.Second)
		}
		peers := make([]*rawPeer, 0, len(status.Peer)+1)
		peers = append(peers, status.Self)
		for _, peer := range status.Peer {
			peers = append(peers, peer)
		}
		user := userForAddress(peers, ip)
		if user == 0 {
			return nil, nil
		}
		owners := []string{}
		for _, peer := range peers {
			if peer == nil || peer.UserID != user || len(peer.Tags) != 0 {
				continue
			}
			if owner := origins[strings.TrimSuffix(peer.DNSName, ".")]; owner != "" {
				owners = append(owners, owner)
			}
		}
		sort.Strings(owners)
		return owners, nil
	}
}

func userForAddress(peers []*rawPeer, ip netip.Addr) uint64 {
	for _, peer := range peers {
		if peer == nil || len(peer.Tags) != 0 {
			continue
		}
		for _, address := range peer.TailscaleIPs {
			if parsed, err := netip.ParseAddr(address); err == nil && parsed == ip {
				return peer.UserID
			}
		}
	}
	return 0
}
