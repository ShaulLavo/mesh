package dnsname

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/tailnet"
)

var tailnetIPv6Prefix = netip.MustParsePrefix("fd7a:115c:a1e0::/48")

// PublicCertificateManagerConfig wires the independent public wildcard
// issuance and exact edge distribution loop. It never reconciles A records.
type PublicCertificateManagerConfig struct {
	Renewer       CertificateRenewer
	Distributor   CertificateDistributor
	Target        PublicEdgeTarget
	DiscoverSelf  func(context.Context) (tailnet.Peer, error)
	DiscoverPeers func(context.Context) ([]tailnet.Peer, error)
	PassTimeout   time.Duration
}

// PublicCertificateManager renews *.shaulavo.dev and distributes it only to
// the pinned direct-TLS edge. It is scheduled independently from private DNS.
type PublicCertificateManager struct {
	renewer       CertificateRenewer
	distributor   CertificateDistributor
	target        PublicEdgeTarget
	discoverSelf  func(context.Context) (tailnet.Peer, error)
	discoverPeers func(context.Context) ([]tailnet.Peer, error)
	passTimeout   time.Duration
	wait          func(context.Context, time.Duration) bool
}

func NewPublicCertificateManager(config PublicCertificateManagerConfig) (*PublicCertificateManager, error) {
	if config.Renewer == nil {
		return nil, errors.New("dnsname: public certificate renewer is nil")
	}
	if config.DiscoverSelf == nil || config.DiscoverPeers == nil {
		return nil, errors.New("dnsname: public certificate Tailscale discovery is incomplete")
	}
	if err := validatePublicEdgeTarget(config.Target); err != nil {
		return nil, fmt.Errorf("dnsname: public edge target: %w", err)
	}
	if config.PassTimeout == 0 {
		config.PassTimeout = defaultReconcileTimeout
	}
	if config.PassTimeout <= 0 || config.PassTimeout > maximumReconcileTimeout {
		return nil, fmt.Errorf("dnsname: public certificate pass timeout %s is outside (0,%s]", config.PassTimeout, maximumReconcileTimeout)
	}
	return &PublicCertificateManager{
		renewer: config.Renewer, distributor: config.Distributor, target: config.Target,
		discoverSelf: config.DiscoverSelf, discoverPeers: config.DiscoverPeers,
		passTimeout: config.PassTimeout, wait: waitForReconcile,
	}, nil
}

// RunOnce renews independently of discovery and redistributes every usable
// current bundle so an edge that was offline at issuance converges later.
func (m *PublicCertificateManager) RunOnce(ctx context.Context, forceRenewal bool) error {
	if ctx == nil {
		return errors.New("dnsname: reconcile public certificate with nil context")
	}
	ctx, cancel := context.WithTimeout(ctx, m.passTimeout)
	defer cancel()
	self, selfErr := m.discoverSelf(ctx)
	peers, peersErr := m.discoverPeers(ctx)
	passErrors := []error{
		wrapOptionalError("dnsname: discover Tailscale self for public edge", selfErr),
		wrapOptionalError("dnsname: discover Tailscale peers for public edge", peersErr),
	}
	var discovered []tailnet.Peer
	if selfErr == nil {
		discovered = append(discovered, self)
	}
	if peersErr == nil {
		discovered = append(discovered, peers...)
	}
	var matched *tailnet.Peer
	for index := range discovered {
		if discovered[index].Name != m.target.TailscaleName {
			continue
		}
		if matched != nil {
			passErrors = append(passErrors, errors.New("dnsname: Tailscale discovery returned duplicate public edge name"))
			matched = nil
			break
		}
		matched = &discovered[index]
	}
	var targets []OriginTarget
	if matched == nil {
		passErrors = append(passErrors, errors.New("dnsname: public edge Tailscale peer is absent or ambiguous"))
	} else if !matched.Online {
		passErrors = append(passErrors, errors.New("dnsname: public edge is offline in Tailscale"))
	} else if address, err := publicCertificateAddress(matched.Addrs); err != nil {
		passErrors = append(passErrors, fmt.Errorf("dnsname: public edge: %w", err))
	} else {
		targets = []OriginTarget{{
			Name: "public-edge", Identity: m.target.Identity,
			Endpoint: "ws://" + netip.AddrPortFrom(address, m.target.ControlPort).String() + m.target.WebSocketPath,
		}}
	}
	bundle, _, renewErr := m.renewer.Renew(ctx, forceRenewal)
	if renewErr != nil {
		passErrors = append(passErrors, fmt.Errorf("dnsname: renew public wildcard certificate: %w", renewErr))
	}
	if m.distributor != nil && len(targets) > 0 && (renewErr == nil || bundle.Fingerprint != "") {
		if err := m.distributor.Distribute(ctx, bundle, targets); err != nil {
			passErrors = append(passErrors, fmt.Errorf("dnsname: distribute public wildcard certificate: %w", err))
		}
	}
	return errors.Join(passErrors...)
}

func (m *PublicCertificateManager) Run(ctx context.Context, interval time.Duration, report func(error)) error {
	if ctx == nil {
		return errors.New("dnsname: run public certificate loop with nil context")
	}
	if interval <= 0 {
		return errors.New("dnsname: public certificate interval must be positive")
	}
	if report == nil {
		report = func(error) {}
	}
	failures := 0
	for {
		err := m.RunOnce(ctx, false)
		if ctx.Err() != nil {
			return nil
		}
		delay := interval
		if err != nil {
			report(err)
			failures++
			delay = min(failureRetryDelay(failures), interval)
		} else {
			failures = 0
		}
		if !m.wait(ctx, delay) {
			return nil
		}
	}
}

func publicCertificateAddress(values []string) (netip.Addr, error) {
	addresses := make([]netip.Addr, 0, len(values))
	for _, value := range values {
		address, err := netip.ParseAddr(value)
		if err != nil {
			return netip.Addr{}, errors.New("Tailscale returned a malformed public edge address")
		}
		address = address.Unmap()
		if !tailnetIPv4Prefix.Contains(address) && !tailnetIPv6Prefix.Contains(address) {
			return netip.Addr{}, errors.New("Tailscale returned a public edge address outside its ranges")
		}
		addresses = append(addresses, address)
	}
	if len(addresses) == 0 {
		return netip.Addr{}, errors.New("public edge has no Tailscale address")
	}
	slices.SortFunc(addresses, func(left, right netip.Addr) int {
		if left.Is4() != right.Is4() {
			if left.Is4() {
				return -1
			}
			return 1
		}
		return left.Compare(right)
	})
	return addresses[0], nil
}
