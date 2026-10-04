package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/usagefeed"
)

const (
	hostConfigVersion      = 1
	hostConfigName         = "hosts.json"
	maximumConfiguredHosts = 256
)

// HostRecord is the local address book entry for one adopted Mesh host.
type HostRecord struct {
	MachineName   string `json:"-"`
	NameRevision  uint64 `json:"-"`
	NameVerified  bool   `json:"-"`
	NameConflict  bool   `json:"-"`
	NamePriority  bool   `json:"-"`
	NameSuffix    string `json:"-"`
	local         bool
	targetName    string
	ID            string   `json:"id"`
	MeshIdentity  string   `json:"meshIdentity"`
	TailscaleName string   `json:"tailscaleName,omitempty"`
	Addresses     []string `json:"addresses,omitempty"`
	Endpoint      string   `json:"endpoint"`
}

type hostConfig struct {
	Version   int                `json:"version"`
	Hosts     []HostRecord       `json:"hosts"`
	Dashboard *DashboardSettings `json:"dashboard,omitempty"`
}

// ConfigPath returns the host address book path. MESH_CONFIG_DIR overrides the
// platform location so tests and portable installations can isolate it.
func ConfigPath() (string, error) {
	dir := os.Getenv("MESH_CONFIG_DIR")
	if dir == "" {
		dir = os.Getenv("XDG_CONFIG_HOME")
		if dir != "" {
			dir = filepath.Join(dir, "mesh")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("locate home directory: %w", err)
			}
			dir = filepath.Join(home, ".config", "mesh")
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve Mesh config directory %s: %w", dir, err)
	}
	return filepath.Join(abs, hostConfigName), nil
}

// TailscaleAuthKeyPath is where mesh looks for an auth key when none is named.
// It sits beside hosts.json, which every mesh add already prints, rather than
// as a loose dotfile in the home directory.
func TailscaleAuthKeyPath() (string, error) {
	config, err := ConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(config), "tailscale-auth-key"), nil
}

// LoadHosts reads and validates the local host address book.
func LoadHosts() ([]HostRecord, error) {
	config, err := loadHostConfig()
	if err != nil {
		return nil, err
	}
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	for i := range config.Hosts {
		host := &config.Hosts[i]
		if _, err := identity.IdentityKey(host.ID); err != nil {
			continue
		}
		claim, err := machinename.CachedClaim(filepath.Dir(path), host.ID)
		if err != nil {
			return nil, fmt.Errorf("load cached machine name: %w", err)
		}
		host.MachineName, host.NameRevision = claim.MachineName, claim.Revision
	}
	ProjectHostNames(config.Hosts)
	sortHosts(config.Hosts)
	return config.Hosts, nil
}

func loadHostConfig() (hostConfig, error) {
	path, err := ConfigPath()
	if err != nil {
		return hostConfig{}, err
	}
	contents, err := os.ReadFile(path) //nolint:gosec // path is Mesh's fixed per-user host configuration file
	if errors.Is(err, os.ErrNotExist) {
		return hostConfig{Version: hostConfigVersion}, nil
	}
	if err != nil {
		return hostConfig{}, fmt.Errorf("read host config %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var config hostConfig
	if err := decoder.Decode(&config); err != nil {
		return hostConfig{}, fmt.Errorf("parse host config %s: %w; remove the unrecognized field from this file and retry", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return hostConfig{}, fmt.Errorf("parse host config %s: trailing data: %w", path, err)
	}
	return validateHostConfig(config, path)
}

func validateHostConfig(config hostConfig, path string) (hostConfig, error) {
	if config.Version != hostConfigVersion {
		return hostConfig{}, fmt.Errorf("parse host config %s: version %d is unsupported", path, config.Version)
	}
	if len(config.Hosts) > maximumConfiguredHosts {
		return hostConfig{}, fmt.Errorf("parse host config %s: host count %d exceeds %d", path, len(config.Hosts), maximumConfiguredHosts)
	}
	identities := make(map[string]bool, len(config.Hosts))
	for i, record := range config.Hosts {
		host, err := validateHostRecord(record)
		if err != nil {
			return hostConfig{}, fmt.Errorf("parse host config %s: host %d: %w", path, i+1, err)
		}
		if identities[host.ID] {
			return hostConfig{}, fmt.Errorf("parse host config %s: duplicate host ID %q", path, host.ID)
		}
		identities[host.ID] = true
		config.Hosts[i] = host
	}
	if config.Dashboard != nil && config.Dashboard.UsageFeedURL != "" {
		if err := usagefeed.ValidateURL(config.Dashboard.UsageFeedURL); err != nil {
			return hostConfig{}, fmt.Errorf("dashboard configuration: %w", err)
		}
	}
	sortHosts(config.Hosts)
	return config, nil
}

// SaveHost atomically adds or replaces one adopted host in the address book.
func SaveHost(record HostRecord) error {
	host, err := validateHostRecord(record)
	if err != nil {
		return err
	}
	config, err := loadHostConfig()
	if err != nil {
		return err
	}
	hosts := config.Hosts
	replaced := false
	for i, existing := range hosts {
		if existing.ID == host.ID {
			hosts[i] = host
			replaced = true
		}
	}
	if !replaced {
		hosts = append(hosts, host)
	}
	sortHosts(hosts)
	config.Hosts = hosts
	return writeHostConfig(config)
}

func validateHostRecord(record HostRecord) (HostRecord, error) {
	record.ID = strings.TrimSpace(record.ID)
	record.MeshIdentity = strings.TrimSpace(record.MeshIdentity)
	record.TailscaleName = strings.TrimSpace(record.TailscaleName)
	record.Endpoint = strings.TrimSpace(record.Endpoint)
	if record.ID == "" {
		return HostRecord{}, fmt.Errorf("host %q has no stable ID", record.ID)
	}
	if record.MeshIdentity == "" {
		return HostRecord{}, fmt.Errorf("host %q has no Mesh identity", record.ID)
	}
	endpoint, err := url.Parse(record.Endpoint)
	if err != nil || (endpoint.Scheme != "ws" && endpoint.Scheme != "wss") || endpoint.Host == "" || endpoint.Path == "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return HostRecord{}, fmt.Errorf("host %q has invalid WebSocket endpoint %q", record.ID, record.Endpoint)
	}
	record.Addresses = append([]string(nil), record.Addresses...)
	for i := range record.Addresses {
		record.Addresses[i] = strings.TrimSpace(record.Addresses[i])
		if _, err := netip.ParseAddr(record.Addresses[i]); err != nil {
			return HostRecord{}, fmt.Errorf("host %q has invalid Tailscale address %q: %w", record.ID, record.Addresses[i], err)
		}
	}
	return record, nil
}

func writeHostConfig(config hostConfig) error {
	if len(config.Hosts) > maximumConfiguredHosts {
		return fmt.Errorf("host count %d exceeds %d", len(config.Hosts), maximumConfiguredHosts)
	}
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create Mesh config directory %s: %w", dir, err)
	}
	contents, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encode host config: %w", err)
	}
	contents = append(contents, '\n')
	temporary, err := os.CreateTemp(dir, ".hosts-*.json")
	if err != nil {
		return fmt.Errorf("create temporary host config: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath) //nolint:errcheck // best-effort cleanup after atomic replacement
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary host config: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary host config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary host config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary host config: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish host config %s: %w", path, err)
	}
	return nil
}

func sortHosts(hosts []HostRecord) {
	sort.Slice(hosts, func(i, j int) bool {
		if hosts[i].MachineName != hosts[j].MachineName {
			return hosts[i].MachineName < hosts[j].MachineName
		}
		return hosts[i].ID < hosts[j].ID
	})
}

// ArgumentTarget is either a machine name or exact host ID or a syntactically valid session ID.
type ArgumentTarget struct {
	Host      *HostRecord
	SessionID string
}

// ResolveArgument classifies the root command's positional argument.
func ResolveArgument(value string, hosts []HostRecord) (ArgumentTarget, error) {
	for _, host := range hosts {
		if value != "" && value == host.ID {
			return ArgumentTarget{Host: &host}, nil
		}
	}
	if target, matched, err := resolveDeclaredArgument(value, hosts); matched || err != nil {
		return target, err
	}
	id, err := session.ParseID(value)
	if err == nil {
		return ArgumentTarget{SessionID: id}, nil
	}
	return ArgumentTarget{}, fmt.Errorf("%q is neither a declared machine name, exact host ID nor a session ID", value)
}
