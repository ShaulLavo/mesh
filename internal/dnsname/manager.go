package dnsname

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/tailnet"
)

const (
	defaultReconcileTimeout = 25 * time.Minute
	maximumReconcileTimeout = time.Hour
	initialFailureRetry     = 30 * time.Second
	maximumFailureRetry     = 15 * time.Minute
)

// PrivateOrigin maps one stable private DNS label and Mesh identity to its
// current Tailscale peer and direct control listener.
type PrivateOrigin struct {
	Name          string `json:"name"`
	TailscaleName string `json:"tailscaleName"`
	Identity      string `json:"identity"`
	ControlPort   uint16 `json:"controlPort"`
	WebSocketPath string `json:"websocketPath"`
}

// PublicEdgeTarget is the one identity-pinned VPS certificate recipient. It
// deliberately carries no DNS record name; public A records remain outside
// Mesh ownership.
type PublicEdgeTarget struct {
	TailscaleName string `json:"tailscaleName"`
	Identity      string `json:"identity"`
	ControlPort   uint16 `json:"controlPort"`
	WebSocketPath string `json:"websocketPath"`
}

// CertificateRenewer returns the current bundle, issuing when needed.
type CertificateRenewer interface {
	Renew(context.Context, bool) (Bundle, bool, error)
}

// CertificateDistributor sends the current bundle to available origins.
type CertificateDistributor interface {
	Distribute(context.Context, Bundle, []OriginTarget) error
}

// PrivateNamesManagerConfig supplies the stateful actors and boundary
// discovery functions shared by one reconciliation loop.
type PrivateNamesManagerConfig struct {
	Provider      Provider
	Renewer       CertificateRenewer
	Distributor   CertificateDistributor
	Origins       []PrivateOrigin
	DiscoverSelf  func(context.Context) (tailnet.Peer, error)
	DiscoverPeers func(context.Context) ([]tailnet.Peer, error)
	PassTimeout   time.Duration
}

// PrivateNamesManager converges private records, a wildcard certificate, and
// origin installs. It owns no timer until Run is called.
type PrivateNamesManager struct {
	provider      Provider
	renewer       CertificateRenewer
	distributor   CertificateDistributor
	origins       []PrivateOrigin
	discoverSelf  func(context.Context) (tailnet.Peer, error)
	discoverPeers func(context.Context) ([]tailnet.Peer, error)
	passTimeout   time.Duration
	wait          func(context.Context, time.Duration) bool
}

// NewPrivateNamesManager validates the complete desired origin set before any
// discovery, DNS, ACME, or network operation.
func NewPrivateNamesManager(config PrivateNamesManagerConfig) (*PrivateNamesManager, error) {
	if config.Provider == nil {
		return nil, errors.New("dnsname: private-names provider is nil")
	}
	if config.Renewer == nil {
		return nil, errors.New("dnsname: private-names renewer is nil")
	}
	if config.DiscoverSelf == nil || config.DiscoverPeers == nil {
		return nil, errors.New("dnsname: private-names Tailscale discovery is incomplete")
	}
	if config.PassTimeout == 0 {
		config.PassTimeout = defaultReconcileTimeout
	}
	if config.PassTimeout <= 0 || config.PassTimeout > maximumReconcileTimeout {
		return nil, fmt.Errorf("dnsname: private-names pass timeout %s is outside (0,%s]", config.PassTimeout, maximumReconcileTimeout)
	}
	if len(config.Origins) == 0 || len(config.Origins) > maximumDistributionTargets {
		return nil, fmt.Errorf("dnsname: private origin count %d is outside 1..%d", len(config.Origins), maximumDistributionTargets)
	}
	seenNames := make(map[string]struct{}, len(config.Origins))
	seenTailscaleNames := make(map[string]struct{}, len(config.Origins))
	seenIdentities := make(map[string]struct{}, len(config.Origins))
	for index, origin := range config.Origins {
		if err := validatePrivateOrigin(origin); err != nil {
			return nil, fmt.Errorf("dnsname: private origin %d: %w", index, err)
		}
		if _, found := seenNames[origin.Name]; found {
			return nil, fmt.Errorf("dnsname: private origin %d duplicates name %q", index, origin.Name)
		}
		if _, found := seenTailscaleNames[origin.TailscaleName]; found {
			return nil, fmt.Errorf("dnsname: private origin %d duplicates Tailscale name %q", index, origin.TailscaleName)
		}
		if _, found := seenIdentities[origin.Identity]; found {
			return nil, fmt.Errorf("dnsname: private origin %d duplicates identity %q", index, origin.Identity)
		}
		seenNames[origin.Name] = struct{}{}
		seenTailscaleNames[origin.TailscaleName] = struct{}{}
		seenIdentities[origin.Identity] = struct{}{}
	}
	return &PrivateNamesManager{
		provider: config.Provider, renewer: config.Renewer, distributor: config.Distributor,
		origins:      append([]PrivateOrigin(nil), config.Origins...),
		discoverSelf: config.DiscoverSelf, discoverPeers: config.DiscoverPeers,
		passTimeout: config.PassTimeout, wait: waitForReconcile,
	}, nil
}

// RunOnce performs one convergent pass. It always redistributes the returned
// current bundle, including when the certificate did not renew this pass.
func (m *PrivateNamesManager) RunOnce(ctx context.Context, forceRenewal bool) error {
	if ctx == nil {
		return errors.New("dnsname: reconcile private names with nil context")
	}
	ctx, cancel := context.WithTimeout(ctx, m.passTimeout)
	defer cancel()
	self, selfErr := m.discoverSelf(ctx)
	peers, peersErr := m.discoverPeers(ctx)
	var passErrors []error
	passErrors = append(passErrors,
		wrapOptionalError("dnsname: discover Tailscale self", selfErr),
		wrapOptionalError("dnsname: discover Tailscale peers", peersErr),
	)

	peersByName := make(map[string]tailnet.Peer, len(peers)+1)
	ambiguousNames := make(map[string]struct{})
	discovered := make([]tailnet.Peer, 0, len(peers)+1)
	if selfErr == nil {
		discovered = append(discovered, self)
	}
	if peersErr == nil {
		discovered = append(discovered, peers...)
	}
	for _, peer := range discovered {
		if _, ambiguous := ambiguousNames[peer.Name]; ambiguous {
			passErrors = append(passErrors, fmt.Errorf("dnsname: Tailscale discovery returned duplicate peer %q", peer.Name))
			continue
		}
		if _, exists := peersByName[peer.Name]; exists {
			delete(peersByName, peer.Name)
			ambiguousNames[peer.Name] = struct{}{}
			passErrors = append(passErrors, fmt.Errorf("dnsname: Tailscale discovery returned duplicate peer %q", peer.Name))
			continue
		}
		peersByName[peer.Name] = peer
	}

	targets := make([]OriginTarget, 0, len(m.origins))
	for _, origin := range m.origins {
		peer, exists := peersByName[origin.TailscaleName]
		if !exists {
			passErrors = append(passErrors, fmt.Errorf("dnsname: origin %s: Tailscale peer %s is absent", origin.Name, origin.TailscaleName))
			continue
		}
		address, err := tailnetIPv4(peer.Addrs)
		if err != nil {
			passErrors = append(passErrors, fmt.Errorf("dnsname: origin %s: %w", origin.Name, err))
			continue
		}
		if _, err := ReconcileHostA(ctx, m.provider, HostAddress{Name: origin.Name, Address: address}); err != nil {
			passErrors = append(passErrors, fmt.Errorf("dnsname: origin %s: %w", origin.Name, err))
		}
		targets = append(targets, OriginTarget{
			Name: origin.Name, Identity: origin.Identity,
			Endpoint: "ws://" + netip.AddrPortFrom(address, origin.ControlPort).String() + origin.WebSocketPath,
		})
	}

	bundle, _, err := m.renewer.Renew(ctx, forceRenewal)
	if err != nil {
		passErrors = append(passErrors, fmt.Errorf("dnsname: renew private wildcard certificate: %w", err))
	}
	if m.distributor != nil && (err == nil || bundle.Fingerprint != "") {
		if err := m.distributor.Distribute(ctx, bundle, targets); err != nil {
			passErrors = append(passErrors, fmt.Errorf("dnsname: distribute private wildcard certificate: %w", err))
		}
	}
	return errors.Join(passErrors...)
}

// Run reconciles immediately. Successful passes use interval; failed passes
// use a bounded backoff so first-boot and transient failures converge sooner.
func (m *PrivateNamesManager) Run(ctx context.Context, interval time.Duration, report func(error)) error {
	if ctx == nil {
		return errors.New("dnsname: run private-names loop with nil context")
	}
	if interval <= 0 {
		return errors.New("dnsname: private-names interval must be positive")
	}
	if report == nil {
		report = func(error) {}
	}
	consecutiveFailures := 0
	for {
		err := m.RunOnce(ctx, false)
		if ctx.Err() != nil {
			return nil
		}
		delay := interval
		if err != nil {
			report(err)
			consecutiveFailures++
			delay = min(failureRetryDelay(consecutiveFailures), interval)
		} else {
			consecutiveFailures = 0
		}
		if !m.wait(ctx, delay) {
			return nil
		}
	}
}

func failureRetryDelay(consecutiveFailures int) time.Duration {
	if consecutiveFailures <= 1 {
		return initialFailureRetry
	}
	delay := initialFailureRetry
	for range consecutiveFailures - 1 {
		if delay >= maximumFailureRetry/2 {
			return maximumFailureRetry
		}
		delay *= 2
	}
	return min(delay, maximumFailureRetry)
}

func waitForReconcile(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func validatePrivateOrigin(origin PrivateOrigin) error {
	if _, err := privateHostName(origin.Name); err != nil {
		return err
	}
	return validateCertificateTarget(origin.TailscaleName, origin.Identity, origin.ControlPort, origin.WebSocketPath)
}

func validatePublicEdgeTarget(target PublicEdgeTarget) error {
	return validateCertificateTarget(target.TailscaleName, target.Identity, target.ControlPort, target.WebSocketPath)
}

func validateCertificateTarget(tailscaleName, identity string, controlPort uint16, webSocketPath string) error {
	if canonical, err := canonicalDNSName(tailscaleName); err != nil || canonical != tailscaleName {
		return fmt.Errorf("invalid canonical Tailscale name %q", tailscaleName)
	}
	if _, err := decodeIdentity("certificate target", identity); err != nil {
		return err
	}
	if controlPort == 0 {
		return errors.New("control port must be non-zero")
	}
	parsed, err := url.Parse(webSocketPath)
	if err != nil || webSocketPath == "" || webSocketPath[0] != '/' || strings.Contains(webSocketPath, "\\") || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Opaque != "" || parsed.ForceQuery ||
		parsed.EscapedPath() != webSocketPath || path.Clean(webSocketPath) != webSocketPath {
		return fmt.Errorf("WebSocket path %q must be a clean absolute path without query or fragment", webSocketPath)
	}
	return nil
}

func tailnetIPv4(values []string) (netip.Addr, error) {
	addresses := make([]netip.Addr, 0, len(values))
	for _, value := range values {
		address, err := netip.ParseAddr(value)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("parse Tailscale address %q: %w", value, err)
		}
		address = address.Unmap()
		if address.Is4() && tailnetIPv4Prefix.Contains(address) {
			addresses = append(addresses, address)
		}
	}
	if len(addresses) == 0 {
		return netip.Addr{}, fmt.Errorf("peer has no tailnet IPv4 address in %s", tailnetIPv4Prefix)
	}
	slices.SortFunc(addresses, func(left, right netip.Addr) int { return left.Compare(right) })
	return addresses[0], nil
}

func wrapOptionalError(prefix string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", prefix, err)
}
