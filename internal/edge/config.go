package edge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const maximumConfigBytes = 1 << 20

// RuntimeConfig fixes the public listener, edge identity pins, and complete
// origin allowlist. It contains no Cloudflare or certificate private key.
type RuntimeConfig struct {
	Mode                 Mode
	ListenAddress        string
	CertificateRenewerID string
	Origins              []OriginConfig
}

type runtimeConfigFile struct {
	Mode                 Mode           `json:"mode"`
	ListenAddress        string         `json:"listenAddress"`
	CertificateRenewerID string         `json:"certificateRenewerId"`
	Origins              []OriginConfig `json:"origins"`
}

// LoadRuntimeConfig strictly parses one non-secret VPS edge configuration.
func LoadRuntimeConfig(path string) (RuntimeConfig, error) {
	var raw runtimeConfigFile
	if err := decodeConfigFile(path, &raw); err != nil {
		return RuntimeConfig{}, err
	}
	if raw.Mode == "" {
		raw.Mode = ModeProxy
	}
	if raw.ListenAddress == "" {
		if raw.Mode == ModeProxy {
			raw.ListenAddress = DefaultPublicListenAddress
		} else {
			raw.ListenAddress = DefaultDirectListenAddress
		}
	}
	if err := validateListenAddress(raw.Mode, raw.ListenAddress); err != nil {
		return RuntimeConfig{}, err
	}
	switch raw.Mode {
	case ModeProxy:
		if raw.CertificateRenewerID != "" {
			return RuntimeConfig{}, errors.New("edge: proxy mode must not configure a certificate renewer")
		}
	case ModeDirectTLS:
		if _, err := parseIdentity("certificate renewer", raw.CertificateRenewerID); err != nil {
			return RuntimeConfig{}, err
		}
	default:
		return RuntimeConfig{}, fmt.Errorf("edge: unsupported listener mode %q", raw.Mode)
	}
	if len(raw.Origins) == 0 || len(raw.Origins) > maximumOrigins {
		return RuntimeConfig{}, fmt.Errorf("edge: origin allowlist count %d is outside 1..%d", len(raw.Origins), maximumOrigins)
	}
	identities := make(map[string]struct{}, len(raw.Origins))
	names := make(map[string]struct{}, len(raw.Origins))
	for index, origin := range raw.Origins {
		if err := validateOriginConfig(origin); err != nil {
			return RuntimeConfig{}, fmt.Errorf("edge: origin allowlist entry %d: %w", index, err)
		}
		if _, exists := identities[origin.Identity]; exists {
			return RuntimeConfig{}, errors.New("edge: origin allowlist contains a duplicate identity")
		}
		if _, exists := names[origin.TailscaleName]; exists {
			return RuntimeConfig{}, errors.New("edge: origin allowlist contains a duplicate Tailscale name")
		}
		identities[origin.Identity] = struct{}{}
		names[origin.TailscaleName] = struct{}{}
	}
	return RuntimeConfig{
		Mode: raw.Mode, ListenAddress: raw.ListenAddress, CertificateRenewerID: raw.CertificateRenewerID,
		Origins: append([]OriginConfig(nil), raw.Origins...),
	}, nil
}

// LoadTargetConfig strictly parses one origin's pinned public-edge target.
func LoadTargetConfig(path string) (TargetConfig, error) {
	var target TargetConfig
	if err := decodeConfigFile(path, &target); err != nil {
		return TargetConfig{}, err
	}
	if err := validateTargetConfig(target); err != nil {
		return TargetConfig{}, err
	}
	return target, nil
}

func decodeConfigFile(path string, destination any) error {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("edge: config path must be clean and absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("edge: open config %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // the read result is already decided
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("edge: inspect config %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumConfigBytes {
		return fmt.Errorf("edge: config %s must be a non-empty regular file no larger than %d bytes", path, maximumConfigBytes)
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximumConfigBytes+1))
	if err != nil {
		return fmt.Errorf("edge: read config %s: %w", path, err)
	}
	if len(contents) > maximumConfigBytes {
		return fmt.Errorf("edge: config %s exceeds %d bytes", path, maximumConfigBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("edge: parse config %s: %w", path, err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return fmt.Errorf("edge: parse config %s: %w", path, err)
	}
	return nil
}

func validateListenAddress(mode Mode, value string) error {
	host, portText, err := net.SplitHostPort(value)
	if err != nil || portText == "" {
		return errors.New("edge: listen address must contain a canonical numeric port")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 || strconv.FormatUint(port, 10) != portText {
		return errors.New("edge: listen address port is not canonical")
	}
	if host == "" {
		if mode != ModeDirectTLS || value != ":"+portText {
			return errors.New("edge: only direct TLS may bind all interfaces")
		}
		return nil
	}
	address, err := netip.ParseAddr(host)
	if err != nil || address.Zone() != "" || address.String() != host {
		return errors.New("edge: listen address host must be one canonical numeric address")
	}
	if mode == ModeProxy && !address.IsLoopback() {
		return errors.New("edge: proxy mode must bind a loopback address")
	}
	if mode == ModeDirectTLS && !validPublicListenAddress(address) {
		return errors.New("edge: direct TLS must bind an unspecified or public unicast address outside private and Tailscale ranges")
	}
	return nil
}

func validPublicListenAddress(address netip.Addr) bool {
	address = address.Unmap()
	if address.IsUnspecified() {
		return true
	}
	return address.IsGlobalUnicast() &&
		!address.IsPrivate() &&
		!tailscaleIPv4.Contains(address) &&
		!tailscaleIPv6.Contains(address)
}
