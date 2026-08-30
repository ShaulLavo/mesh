package dnsname

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/tailnet"
)

const (
	privateNamesConfigMaximum = 1 << 20
	cloudflareTokenMaximum    = 4 << 10
	defaultRenewalInterval    = 12 * time.Hour
	minimumRenewalInterval    = 5 * time.Minute
	maximumRenewalInterval    = 7 * 24 * time.Hour
)

// RenewalEnvironment separates ACME accounts and certificate state.
type RenewalEnvironment string

const (
	EnvironmentLive    RenewalEnvironment = "live"
	EnvironmentStaging RenewalEnvironment = "staging"
)

// PrivateNamesConfig is the validated, non-secret Pi configuration. TokenFile
// points to a separate exact-0600 file and never contains the token itself.
type PrivateNamesConfig struct {
	ZoneID       string
	TokenFile    string
	ACMEEmail    string
	DirectoryURL string
	Environment  RenewalEnvironment
	AcceptTerms  bool
	Interval     time.Duration
	Origins      []PrivateOrigin
	PublicEdge   *PublicEdgeTarget
}

type privateNamesConfigFile struct {
	ZoneID       string            `json:"zoneId"`
	TokenFile    string            `json:"tokenFile"`
	ACMEEmail    string            `json:"acmeEmail"`
	DirectoryURL string            `json:"directoryUrl"`
	AcceptTerms  bool              `json:"acceptTerms"`
	Interval     string            `json:"interval"`
	Origins      []PrivateOrigin   `json:"origins"`
	PublicEdge   *PublicEdgeTarget `json:"publicEdge"`
}

// LoadPrivateNamesConfig strictly parses one Pi configuration file without
// reading its Cloudflare token.
func LoadPrivateNamesConfig(configPath string) (PrivateNamesConfig, error) {
	if strings.TrimSpace(configPath) == "" {
		return PrivateNamesConfig{}, errors.New("dnsname: private-names config path is empty")
	}
	file, err := os.Open(configPath) //nolint:gosec // opening the operator-selected private-names configuration is this boundary's purpose
	if err != nil {
		return PrivateNamesConfig{}, fmt.Errorf("dnsname: open private-names config %s: %w", configPath, err)
	}
	defer file.Close() //nolint:errcheck // read result is already decided
	info, err := file.Stat()
	if err != nil {
		return PrivateNamesConfig{}, fmt.Errorf("dnsname: inspect private-names config %s: %w", configPath, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > privateNamesConfigMaximum {
		return PrivateNamesConfig{}, fmt.Errorf("dnsname: private-names config %s must be a non-empty regular file no larger than %d bytes", configPath, privateNamesConfigMaximum)
	}
	contents, err := io.ReadAll(io.LimitReader(file, privateNamesConfigMaximum+1))
	if err != nil {
		return PrivateNamesConfig{}, fmt.Errorf("dnsname: read private-names config %s: %w", configPath, err)
	}
	if len(contents) > privateNamesConfigMaximum {
		return PrivateNamesConfig{}, fmt.Errorf("dnsname: private-names config %s exceeds %d bytes", configPath, privateNamesConfigMaximum)
	}
	var raw privateNamesConfigFile
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return PrivateNamesConfig{}, fmt.Errorf("dnsname: parse private-names config %s: %w", configPath, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return PrivateNamesConfig{}, fmt.Errorf("dnsname: parse private-names config %s: %w", configPath, err)
	}
	interval := defaultRenewalInterval
	if raw.Interval != "" {
		interval, err = time.ParseDuration(raw.Interval)
		if err != nil {
			return PrivateNamesConfig{}, fmt.Errorf("dnsname: parse private-names interval %q: %w", raw.Interval, err)
		}
	}
	if interval < minimumRenewalInterval || interval > maximumRenewalInterval {
		return PrivateNamesConfig{}, fmt.Errorf("dnsname: private-names interval %s is outside [%s,%s]", interval, minimumRenewalInterval, maximumRenewalInterval)
	}
	environment, err := environmentForDirectoryURL(raw.DirectoryURL)
	if err != nil {
		return PrivateNamesConfig{}, err
	}
	if strings.TrimSpace(raw.ZoneID) == "" || strings.TrimSpace(raw.ZoneID) != raw.ZoneID || len(raw.ZoneID) > 32 {
		return PrivateNamesConfig{}, errors.New("dnsname: Cloudflare zone ID is empty, non-canonical, or longer than 32 characters")
	}
	if !filepath.IsAbs(raw.TokenFile) || filepath.Clean(raw.TokenFile) != raw.TokenFile {
		return PrivateNamesConfig{}, errors.New("dnsname: Cloudflare token file must be a clean absolute path")
	}
	if strings.TrimSpace(raw.ACMEEmail) == "" || strings.TrimSpace(raw.ACMEEmail) != raw.ACMEEmail || strings.ContainsAny(raw.ACMEEmail, "\r\n") {
		return PrivateNamesConfig{}, errors.New("dnsname: ACME account email is empty or invalid")
	}
	if len(raw.Origins) == 0 || len(raw.Origins) > maximumDistributionTargets {
		return PrivateNamesConfig{}, fmt.Errorf("dnsname: private origin count %d is outside 1..%d", len(raw.Origins), maximumDistributionTargets)
	}
	for index, origin := range raw.Origins {
		if err := validatePrivateOrigin(origin); err != nil {
			return PrivateNamesConfig{}, fmt.Errorf("dnsname: private origin %d: %w", index, err)
		}
	}
	if raw.PublicEdge != nil {
		if err := validatePublicEdgeTarget(*raw.PublicEdge); err != nil {
			return PrivateNamesConfig{}, fmt.Errorf("dnsname: public edge: %w", err)
		}
	}
	var publicEdge *PublicEdgeTarget
	if raw.PublicEdge != nil {
		copyTarget := *raw.PublicEdge
		publicEdge = &copyTarget
	}
	return PrivateNamesConfig{
		ZoneID: raw.ZoneID, TokenFile: raw.TokenFile, ACMEEmail: raw.ACMEEmail, DirectoryURL: raw.DirectoryURL, Environment: environment,
		AcceptTerms: raw.AcceptTerms, Interval: interval, Origins: append([]PrivateOrigin(nil), raw.Origins...), PublicEdge: publicEdge,
	}, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func readCloudflareToken(path string) (string, error) {
	contents, err := readSecureFile(path, cloudflareTokenMaximum)
	if err != nil {
		return "", fmt.Errorf("dnsname: read Cloudflare token file %s: %w", path, err)
	}
	text := string(contents)
	token := strings.TrimSuffix(text, "\n")
	if token == "" || strings.ContainsAny(token, "\x00\r\n\t ") {
		return "", fmt.Errorf("dnsname: Cloudflare token file %s contains an empty or invalid token", path)
	}
	return token, nil
}

func environmentForDirectoryURL(directoryURL string) (RenewalEnvironment, error) {
	switch directoryURL {
	case LetsEncryptProductionURL:
		return EnvironmentLive, nil
	case LetsEncryptStagingURL:
		return EnvironmentStaging, nil
	default:
		return "", fmt.Errorf("dnsname: ACME directory URL must be exactly %q or %q", LetsEncryptProductionURL, LetsEncryptStagingURL)
	}
}

func validateRenewalEnvironment(environment RenewalEnvironment) error {
	if environment != EnvironmentLive && environment != EnvironmentStaging {
		return fmt.Errorf("dnsname: certificate environment %q must be %q or %q", environment, EnvironmentLive, EnvironmentStaging)
	}
	return nil
}

// PrivateNamesRuntimeOptions selects one isolated environment and whether its
// current bundle is distributed to the matching origin slot.
type PrivateNamesRuntimeOptions struct {
	StateDir      string
	Signer        ed25519.PrivateKey
	DirectoryURL  string
	AcceptTerms   bool
	Distribute    bool
	DiscoverSelf  func(context.Context) (tailnet.Peer, error)
	DiscoverPeers func(context.Context) ([]tailnet.Peer, error)
}

// PrivateNamesRuntime is one fully wired operational reconciliation loop.
type PrivateNamesRuntime struct {
	Manager       *PrivateNamesManager
	PublicManager *PublicCertificateManager
	Environment   RenewalEnvironment
	Interval      time.Duration
}

// NewPrivateNamesRuntime wires Cloudflare, authoritative DNS observation,
// Let's Encrypt, Tailscale discovery, and bounded origin distribution.
func NewPrivateNamesRuntime(configPath string, options PrivateNamesRuntimeOptions) (*PrivateNamesRuntime, error) {
	config, err := LoadPrivateNamesConfig(configPath)
	if err != nil {
		return nil, err
	}
	directoryURL := config.DirectoryURL
	if options.DirectoryURL != "" {
		directoryURL = options.DirectoryURL
	}
	environment, err := environmentForDirectoryURL(directoryURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(options.StateDir) == "" {
		return nil, errors.New("dnsname: private-names state directory is empty")
	}
	stateDir, err := filepath.Abs(options.StateDir)
	if err != nil {
		return nil, fmt.Errorf("dnsname: resolve private-names state directory: %w", err)
	}
	token, err := readCloudflareToken(config.TokenFile)
	if err != nil {
		return nil, err
	}
	provider, err := NewCloudflare(CloudflareConfig{ZoneID: config.ZoneID, APIToken: token})
	if err != nil {
		return nil, err
	}
	environmentState := filepath.Join(stateDir, "private-names", string(environment))
	issuer, err := NewIssuer(IssuerConfig{
		DirectoryURL: directoryURL, Email: config.ACMEEmail, StateDir: environmentState, Name: WildcardName,
		AcceptTerms: config.AcceptTerms || options.AcceptTerms,
		Solver:      DNS01Solver{Provider: provider, Observer: AuthoritativeObserver{}, Zone: Zone},
	})
	if err != nil {
		return nil, err
	}
	var distributor CertificateDistributor
	if options.Distribute {
		distributor, err = NewDistributor(DistributorConfig{Profile: ProfilePrivateOrigin, Signer: options.Signer, Environment: environment})
		if err != nil {
			return nil, err
		}
	}
	discoverSelf := options.DiscoverSelf
	if discoverSelf == nil {
		discoverSelf = tailnet.Self
	}
	discoverPeers := options.DiscoverPeers
	if discoverPeers == nil {
		discoverPeers = tailnet.Peers
	}
	manager, err := NewPrivateNamesManager(PrivateNamesManagerConfig{
		Provider: provider, Renewer: issuer, Distributor: distributor, Origins: config.Origins,
		DiscoverSelf: discoverSelf, DiscoverPeers: discoverPeers,
	})
	if err != nil {
		return nil, err
	}
	var publicManager *PublicCertificateManager
	if config.PublicEdge != nil {
		publicState := filepath.Join(stateDir, "public-edge", string(environment))
		publicIssuer, err := NewIssuer(IssuerConfig{
			DirectoryURL: directoryURL, Email: config.ACMEEmail, StateDir: publicState, Name: PublicWildcardName,
			AcceptTerms: config.AcceptTerms || options.AcceptTerms,
			Solver:      DNS01Solver{Provider: provider, Observer: AuthoritativeObserver{}, Zone: Zone},
		})
		if err != nil {
			return nil, err
		}
		var publicDistributor CertificateDistributor
		if options.Distribute {
			publicDistributor, err = NewDistributor(DistributorConfig{Profile: ProfilePublicEdge, Signer: options.Signer, Environment: environment})
			if err != nil {
				return nil, err
			}
		}
		publicManager, err = NewPublicCertificateManager(PublicCertificateManagerConfig{
			Renewer: publicIssuer, Distributor: publicDistributor, Target: *config.PublicEdge,
			DiscoverSelf: discoverSelf, DiscoverPeers: discoverPeers,
		})
		if err != nil {
			return nil, err
		}
	}
	return &PrivateNamesRuntime{Manager: manager, PublicManager: publicManager, Environment: environment, Interval: config.Interval}, nil
}
