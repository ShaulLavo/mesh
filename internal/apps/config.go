package apps

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

const maximumRegistryConfigBytes = 1 << 20

type Peer struct {
	Identity      string `json:"identity"`
	TailscaleName string `json:"tailscaleName"`
	ControlPort   uint16 `json:"controlPort"`
	WebSocketPath string `json:"websocketPath"`
}
type RegistryHostConfig struct {
	ListenAddress        string `json:"listenAddress"`
	CertificateRenewerID string `json:"certificateRenewerId"`
	Origins              []Peer `json:"origins"`
}

func LoadTargetConfig(path string) (Peer, error) {
	var target Peer
	if err := decodeConfigFile(path, &target); err != nil {
		return Peer{}, err
	}
	return target, validatePeer(target)
}
func LoadRegistryConfig(path string) (RegistryHostConfig, error) {
	var config RegistryHostConfig
	if err := decodeConfigFile(path, &config); err != nil {
		return config, err
	}
	if config.ListenAddress == "" {
		config.ListenAddress = "127.0.0.1:8445"
	}
	address, err := netip.ParseAddrPort(config.ListenAddress)
	if err != nil || !address.Addr().IsLoopback() || address.Addr().Zone() != "" || address.Port() == 0 || address.String() != config.ListenAddress {
		return config, errors.New("app: registry HTTPS address must be a canonical loopback endpoint")
	}
	if err := validatePeerIdentity(config.CertificateRenewerID); err != nil {
		return config, fmt.Errorf("app: certificate renewer: %w", err)
	}
	if len(config.Origins) == 0 || len(config.Origins) > 256 {
		return config, errors.New("app: registry requires 1..256 origins")
	}
	identities, names := map[string]bool{}, map[string]bool{}
	for _, origin := range config.Origins {
		if err := validatePeer(origin); err != nil {
			return config, err
		}
		if identities[origin.Identity] || names[origin.TailscaleName] {
			return config, errors.New("app: registry origin identity or peer name is duplicated")
		}
		identities[origin.Identity], names[origin.TailscaleName] = true, true
	}
	return config, nil
}
func validatePeer(peer Peer) error {
	if err := validatePeerIdentity(peer.Identity); err != nil {
		return err
	}
	if err := validateTailscaleName(peer.TailscaleName); err != nil {
		return err
	}
	if peer.ControlPort == 0 {
		return errors.New("app: peer control port is zero")
	}
	return validateControlPath(peer.WebSocketPath)
}
func validatePeerIdentity(value string) error {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return errors.New("app: peer identity is not a canonical Ed25519 public key")
	}
	return nil
}
func decodeConfigFile(path string, destination any) error {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("app: config path must be clean and absolute")
	}
	file, err := os.Open(path) //nolint:gosec // reading the operator-selected app registry configuration is the operation requested
	if err != nil {
		return fmt.Errorf("app: open config %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // the read result is already decided
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("app: inspect config %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumRegistryConfigBytes {
		return fmt.Errorf("app: config %s must be a non-empty regular file no larger than %d bytes", path, maximumRegistryConfigBytes)
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximumRegistryConfigBytes+1))
	if err != nil {
		return fmt.Errorf("app: read config %s: %w", path, err)
	}
	if len(contents) > maximumRegistryConfigBytes {
		return fmt.Errorf("app: config %s exceeds %d bytes", path, maximumRegistryConfigBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("app: parse config %s: %w", path, err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return fmt.Errorf("app: parse config %s: %w", path, err)
	}
	return nil
}
