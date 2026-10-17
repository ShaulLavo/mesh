package fleetshare

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
)

func validatePeer(peer Peer) error {
	if _, err := identity.IdentityKey(peer.Identity); err != nil {
		return fmt.Errorf("fleet: invalid controller identity: %w", err)
	}
	return validateEndpoint(peer.Endpoint)
}

func validateEndpoint(value string) error {
	endpoint, err := url.Parse(value)
	if err != nil || len(value) > 4096 || value != strings.TrimSpace(value) {
		return errors.New("fleet: invalid WebSocket endpoint")
	}
	if (endpoint.Scheme != "ws" && endpoint.Scheme != "wss") || endpoint.Hostname() == "" || endpoint.Path == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.Opaque != "" {
		return errors.New("fleet: invalid WebSocket endpoint")
	}
	if strings.HasSuffix(endpoint.Host, ":") {
		return errors.New("fleet: WebSocket endpoint port is empty")
	}
	if endpoint.Port() != "" {
		port, err := strconv.ParseUint(endpoint.Port(), 10, 16)
		if err != nil || port == 0 {
			return errors.New("fleet: invalid WebSocket endpoint port")
		}
	}
	return nil
}

func validateHosts(hosts []protocol.FleetHost) error {
	if len(hosts) > maximumEntries {
		return fmt.Errorf("fleet: host count exceeds %d", maximumEntries)
	}
	seen := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		if err := validateHost(host, seen); err != nil {
			return err
		}
	}
	return nil
}

func validateHost(host protocol.FleetHost, seen map[string]bool) error {
	if _, err := identity.IdentityKey(host.ID); err != nil {
		return fmt.Errorf("fleet: invalid host identity: %w", err)
	}
	if host.ID != host.MeshIdentity {
		return errors.New("fleet: host ID does not match its pinned identity")
	}
	if seen[host.ID] {
		return fmt.Errorf("fleet: duplicate host %s", host.ID)
	}
	seen[host.ID] = true
	if err := validateEndpoint(host.Endpoint); err != nil {
		return err
	}
	if len(host.TailscaleName) > 253 || len(host.Addresses) > 16 {
		return errors.New("fleet: host address metadata exceeds limits")
	}
	for _, address := range host.Addresses {
		if _, err := netip.ParseAddr(address); err != nil {
			return fmt.Errorf("fleet: invalid host address: %w", err)
		}
	}
	return nil
}
