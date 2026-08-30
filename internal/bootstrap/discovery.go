package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

type tailnetObservation struct {
	Name      string
	Addresses []string
}

type tailscaleStatus struct {
	BackendState string         `json:"BackendState"`
	Self         *tailscalePeer `json:"Self"`
}

type tailscalePeer struct {
	HostName     string   `json:"HostName"`
	DNSName      string   `json:"DNSName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
}

func discoverTailnet(ctx context.Context, remote remoteHost) (tailnetObservation, error) {
	stdout, stderr, err := remote.Run(ctx, "tailscale status --json", nil)
	if err != nil {
		detail := strings.ToLower(string(stdout) + " " + string(stderr) + " " + err.Error())
		if strings.Contains(detail, "not found") || strings.Contains(detail, "not installed") {
			return tailnetObservation{}, diagnostic(DiagnosticTailscaleUnavailable, remoteCommandError("run tailscale status --json", err, stdout, stderr))
		}
		if strings.Contains(detail, "logged out") || strings.Contains(detail, "not logged in") || strings.Contains(detail, "needslogin") {
			return tailnetObservation{}, diagnostic(DiagnosticTailscaleLoggedOut, remoteCommandError("run tailscale status --json", err, stdout, stderr))
		}
		return tailnetObservation{}, diagnostic(DiagnosticTailscaleUnavailable, remoteCommandError("run tailscale status --json", err, stdout, stderr))
	}

	var status tailscaleStatus
	if err := json.Unmarshal(stdout, &status); err != nil {
		return tailnetObservation{}, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("parse tailscale status: %w", err))
	}
	switch status.BackendState {
	case "Running":
	case "NeedsLogin", "NoState":
		return tailnetObservation{}, diagnostic(DiagnosticTailscaleLoggedOut, fmt.Errorf("Tailscale backend state is %s", status.BackendState))
	case "Stopped", "Starting", "NeedsMachineAuth":
		return tailnetObservation{}, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("Tailscale backend state is %s", status.BackendState))
	default:
		return tailnetObservation{}, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("unsupported Tailscale backend state %q", status.BackendState))
	}
	if status.Self == nil {
		return tailnetObservation{}, diagnostic(DiagnosticTailscaleUnavailable, errors.New("tailscale status does not identify the remote host"))
	}
	name := strings.TrimSuffix(status.Self.DNSName, ".")
	if name == "" {
		name = status.Self.HostName
	}
	if name == "" {
		return tailnetObservation{}, diagnostic(DiagnosticTailscaleUnavailable, errors.New("tailscale status has no DNS name or host name"))
	}

	addresses := make([]netip.Addr, 0, len(status.Self.TailscaleIPs))
	seen := make(map[netip.Addr]struct{}, len(status.Self.TailscaleIPs))
	for _, text := range status.Self.TailscaleIPs {
		address, err := netip.ParseAddr(text)
		if err != nil {
			return tailnetObservation{}, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("parse Tailscale address %q: %w", text, err))
		}
		address = address.Unmap()
		if address.IsUnspecified() || address.IsLoopback() || address.IsMulticast() {
			return tailnetObservation{}, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("Tailscale returned non-routable address %s", address))
		}
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	slices.SortFunc(addresses, func(a, b netip.Addr) int {
		if a.Is4() != b.Is4() {
			if a.Is4() {
				return -1
			}
			return 1
		}
		return a.Compare(b)
	})
	result := make([]string, len(addresses))
	for i, address := range addresses {
		result[i] = address.String()
	}
	return tailnetObservation{Name: name, Addresses: result}, nil
}

func checkRemoteClock(ctx context.Context, remote remoteHost, local time.Time) error {
	stdout, stderr, err := remote.Run(ctx, "date +%s", nil)
	if err != nil {
		return diagnostic(DiagnosticClockSkew, remoteCommandError("read remote clock", err, stdout, stderr))
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(string(stdout)), 10, 64)
	if err != nil {
		return diagnostic(DiagnosticClockSkew, fmt.Errorf("parse remote Unix time %q: %w", strings.TrimSpace(string(stdout)), err))
	}
	remoteTime := time.Unix(seconds, 0).UTC()
	skew := local.Sub(remoteTime)
	if skew < 0 {
		skew = -skew
	}
	if skew > maximumClockSkew {
		return diagnostic(DiagnosticClockSkew, fmt.Errorf("remote clock differs from local time by %s", skew.Round(time.Second)))
	}
	return nil
}
