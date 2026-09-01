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

const (
	defaultTailscaleStartingTimeout = 20 * time.Second
	defaultTailscalePollInterval    = 250 * time.Millisecond
	maximumTailscaleNameBytes       = 253
	tailscaleApplicationCLI         = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
)

const tailscaleStatusCommand = `
if [ -d '/Applications/Tailscale.app' ]; then
	if [ ! -x '/Applications/Tailscale.app/Contents/MacOS/Tailscale' ]; then
		printf 'MESH_TAILSCALE_APPLICATION_BROKEN=yes\n' >&2
		exit 126
	fi
	printf 'MESH_TAILSCALE_VARIANT=application\nMESH_TAILSCALE_CLI=/Applications/Tailscale.app/Contents/MacOS/Tailscale\n' >&2
	exec /usr/bin/env TAILSCALE_BE_CLI=1 '/Applications/Tailscale.app/Contents/MacOS/Tailscale' status --json
elif tailscale_cli=$(command -v tailscale 2>/dev/null) && [ -n "$tailscale_cli" ] && [ -x "$tailscale_cli" ]; then
	printf 'MESH_TAILSCALE_VARIANT=system\nMESH_TAILSCALE_CLI=%s\n' "$tailscale_cli" >&2
	exec "$tailscale_cli" status --json
elif [ -x '/opt/homebrew/bin/tailscale' ]; then
	printf 'MESH_TAILSCALE_VARIANT=system\nMESH_TAILSCALE_CLI=/opt/homebrew/bin/tailscale\n' >&2
	exec '/opt/homebrew/bin/tailscale' status --json
elif [ -x '/usr/local/bin/tailscale' ]; then
	printf 'MESH_TAILSCALE_VARIANT=system\nMESH_TAILSCALE_CLI=/usr/local/bin/tailscale\n' >&2
	exec '/usr/local/bin/tailscale' status --json
else
	printf 'MESH_TAILSCALE_MISSING=yes\n' >&2
	exit 127
fi`

type tailscaleState uint8

const (
	tailscaleUnknown tailscaleState = iota
	tailscaleRunning
	tailscaleNeedsLogin
	tailscaleNoState
	tailscaleStopped
	tailscaleStarting
	tailscaleNeedsMachineAuth
	tailscaleMissing
	tailscaleDaemonStopped
)

func (s tailscaleState) String() string {
	switch s {
	case tailscaleRunning:
		return "Running"
	case tailscaleNeedsLogin:
		return "NeedsLogin"
	case tailscaleNoState:
		return "NoState"
	case tailscaleStopped:
		return "Stopped"
	case tailscaleStarting:
		return "Starting"
	case tailscaleNeedsMachineAuth:
		return "NeedsMachineAuth"
	case tailscaleMissing:
		return "Missing"
	case tailscaleDaemonStopped:
		return "DaemonStopped"
	case tailscaleUnknown:
		return "Unknown"
	default:
		return fmt.Sprintf("tailscaleState(%d)", s)
	}
}

type tailscaleObservation struct {
	State   tailscaleState
	Variant tailscaleVariant
	CLIPath string
	Tailnet tailnetObservation
}

type tailscaleVariant uint8

const (
	tailscaleVariantUnknown tailscaleVariant = iota
	tailscaleVariantSystem
	tailscaleVariantApplication
)

func (v tailscaleVariant) detail() string {
	switch v {
	case tailscaleVariantSystem:
		return "headless Tailscale daemon"
	case tailscaleVariantApplication:
		return "Tailscale application"
	case tailscaleVariantUnknown:
		return ""
	default:
		return ""
	}
}

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

func discoverTailnet(ctx context.Context, remote remoteHost) (tailscaleObservation, error) {
	return discoverTailnetWithTiming(ctx, remote, defaultTailscaleStartingTimeout, defaultTailscalePollInterval)
}

func discoverTailnetWithTiming(ctx context.Context, remote remoteHost, timeout, interval time.Duration) (tailscaleObservation, error) {
	discoveryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		observation, err := observeTailscale(discoveryCtx, remote)
		if err != nil || observation.State != tailscaleStarting {
			return observation, err
		}
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
		case <-discoveryCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return observation, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("Tailscale backend state remained Starting until the polling deadline: %w", discoveryCtx.Err()))
		}
	}
}

func observeTailscale(ctx context.Context, remote remoteHost) (tailscaleObservation, error) {
	stdout, stderr, err := remote.Run(ctx, tailscaleStatusCommand, nil)
	variant, cliPath, missing, markerErr := parseTailscaleProbe(stderr)
	if markerErr != nil {
		return tailscaleObservation{}, markerErr
	}
	if missing {
		if err == nil {
			return tailscaleObservation{}, diagnostic(DiagnosticTailscaleUnavailable, errors.New("Tailscale probe reported a missing CLI but exited successfully"))
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return tailscaleObservation{}, diagnostic(DiagnosticTailscaleUnavailable, ctxErr)
		}
		return tailscaleObservation{State: tailscaleMissing}, nil
	}
	if err != nil {
		detail := strings.ToLower(string(stdout) + " " + string(stderr) + " " + err.Error())
		switch {
		case strings.Contains(detail, "failed to connect to local tailscale service"), strings.Contains(detail, "is tailscale running"), strings.Contains(detail, "tailscaled.sock"):
			return tailscaleObservation{State: tailscaleDaemonStopped, Variant: variant, CLIPath: cliPath}, nil
		case strings.Contains(detail, "logged out"), strings.Contains(detail, "not logged in"), strings.Contains(detail, "needslogin"):
			return tailscaleObservation{State: tailscaleNeedsLogin, Variant: variant, CLIPath: cliPath}, nil
		default:
			return tailscaleObservation{}, diagnostic(DiagnosticTailscaleUnavailable, remoteCommandError("run tailscale status --json", err, stdout, stderr))
		}
	}

	var status tailscaleStatus
	if err := json.Unmarshal(stdout, &status); err != nil {
		return tailscaleObservation{}, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("parse tailscale status: %w", err))
	}
	state, err := parseTailscaleState(status.BackendState)
	if err != nil {
		return tailscaleObservation{}, err
	}
	observation := tailscaleObservation{State: state, Variant: variant, CLIPath: cliPath}
	if state != tailscaleRunning {
		return observation, nil
	}
	tailnet, err := parseRunningTailnet(status)
	if err != nil {
		return tailscaleObservation{}, err
	}
	observation.Tailnet = tailnet
	return observation, nil
}

func parseTailscaleProbe(stderr []byte) (tailscaleVariant, string, bool, error) {
	values := markerValues(stderr)
	brokenApplication, hasBrokenApplication := values["MESH_TAILSCALE_APPLICATION_BROKEN"]
	if hasBrokenApplication && brokenApplication != "yes" {
		return tailscaleVariantUnknown, "", false, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("unsupported broken-application marker %q", boundedRemoteOutput([]byte(brokenApplication))))
	}
	if brokenApplication == "yes" {
		return tailscaleVariantUnknown, "", false, diagnostic(DiagnosticTailscaleUnavailable, errors.New("Tailscale.app is installed, but its bundled CLI is not executable; repair or remove the application before provisioning headless Tailscale"))
	}
	missingMarker, hasMissingMarker := values["MESH_TAILSCALE_MISSING"]
	if hasMissingMarker && missingMarker != "yes" {
		return tailscaleVariantUnknown, "", false, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("unsupported Tailscale missing marker %q", boundedRemoteOutput([]byte(missingMarker))))
	}
	if missingMarker == "yes" {
		if values["MESH_TAILSCALE_VARIANT"] != "" || values["MESH_TAILSCALE_CLI"] != "" {
			return tailscaleVariantUnknown, "", false, diagnostic(DiagnosticTailscaleUnavailable, errors.New("Tailscale probe reported both missing and installed markers"))
		}
		return tailscaleVariantUnknown, "", true, nil
	}
	variant := tailscaleVariantUnknown
	switch values["MESH_TAILSCALE_VARIANT"] {
	case "":
	case "system":
		variant = tailscaleVariantSystem
	case "application":
		variant = tailscaleVariantApplication
	default:
		return tailscaleVariantUnknown, "", false, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("unsupported Tailscale variant marker %q", boundedRemoteOutput([]byte(values["MESH_TAILSCALE_VARIANT"]))))
	}
	cliPath := values["MESH_TAILSCALE_CLI"]
	if cliPath != "" && !validRemoteExecutablePath(cliPath) {
		return tailscaleVariantUnknown, "", false, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("invalid Tailscale CLI path %q", boundedRemoteOutput([]byte(cliPath))))
	}
	if variant == tailscaleVariantUnknown && cliPath != "" {
		return tailscaleVariantUnknown, "", false, diagnostic(DiagnosticTailscaleUnavailable, errors.New("Tailscale probe returned a CLI path without a variant"))
	}
	if variant != tailscaleVariantUnknown && cliPath == "" {
		return tailscaleVariantUnknown, "", false, diagnostic(DiagnosticTailscaleUnavailable, errors.New("Tailscale probe returned a variant without a CLI path"))
	}
	if variant == tailscaleVariantApplication && cliPath != tailscaleApplicationCLI {
		return tailscaleVariantUnknown, "", false, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("Tailscale application probe returned unexpected CLI path %q", boundedRemoteOutput([]byte(cliPath))))
	}
	return variant, cliPath, false, nil
}

func parseTailscaleState(value string) (tailscaleState, error) {
	switch value {
	case "Running":
		return tailscaleRunning, nil
	case "NeedsLogin":
		return tailscaleNeedsLogin, nil
	case "NoState":
		return tailscaleNoState, nil
	case "Stopped":
		return tailscaleStopped, nil
	case "Starting":
		return tailscaleStarting, nil
	case "NeedsMachineAuth":
		return tailscaleNeedsMachineAuth, nil
	case "":
		return 0, diagnostic(DiagnosticTailscaleUnavailable, errors.New("tailscale status has no BackendState"))
	default:
		return 0, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("unsupported Tailscale backend state %q", boundedRemoteOutput([]byte(value))))
	}
}

func parseRunningTailnet(status tailscaleStatus) (tailnetObservation, error) {
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
	if len(name) > maximumTailscaleNameBytes {
		return tailnetObservation{}, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("tailscale status name is longer than %d bytes", maximumTailscaleNameBytes))
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return tailnetObservation{}, diagnostic(DiagnosticTailscaleUnavailable, errors.New("tailscale status name contains a control character"))
		}
	}

	addresses := make([]netip.Addr, 0, len(status.Self.TailscaleIPs))
	seen := make(map[netip.Addr]struct{}, len(status.Self.TailscaleIPs))
	for _, text := range status.Self.TailscaleIPs {
		address, err := netip.ParseAddr(text)
		if err != nil {
			return tailnetObservation{}, diagnostic(DiagnosticTailscaleUnavailable, fmt.Errorf("parse Tailscale address %q", boundedRemoteOutput([]byte(text))))
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
	secondsText := strings.TrimSpace(string(stdout))
	seconds, err := strconv.ParseInt(secondsText, 10, 64)
	if err != nil {
		return diagnostic(DiagnosticClockSkew, fmt.Errorf("parse remote Unix time %q", boundedRemoteOutput([]byte(secondsText))))
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
