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

	"github.com/shaul/mesh/internal/session"
)

const (
	hostConfigVersion = 1
	hostConfigName    = "hosts.json"
)

var reservedAliases = map[string]struct{}{
	"add": {}, "attach": {}, "completion": {}, "daemon": {}, "help": {},
	"kill": {}, "list": {}, "local": {}, "logs": {}, "man": {},
	"private-names": {}, "session-worker": {}, "sig": {}, "signal": {}, "wake": {},
}

// HostRecord is the local address book entry for one adopted Mesh host.
type HostRecord struct {
	Alias         string   `json:"alias"`
	ID            string   `json:"id"`
	MeshIdentity  string   `json:"meshIdentity"`
	TailscaleName string   `json:"tailscaleName,omitempty"`
	Addresses     []string `json:"addresses,omitempty"`
	Endpoint      string   `json:"endpoint"`
}

type hostConfig struct {
	Version int          `json:"version"`
	Hosts   []HostRecord `json:"hosts"`
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

// ValidateHostAlias normalizes a host alias and rejects names the CLI cannot
// distinguish from a command or a session ID.
func ValidateHostAlias(value string) (string, error) {
	alias := strings.ToLower(strings.TrimSpace(value))
	if alias == "" {
		return "", errors.New("host alias is empty")
	}
	if len(alias) > 63 {
		return "", fmt.Errorf("host alias %q is longer than 63 characters", value)
	}
	for i, character := range []byte(alias) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' && i > 0 && i < len(alias)-1 {
			continue
		}
		return "", fmt.Errorf("host alias %q must contain lowercase letters, digits, or interior hyphens", value)
	}
	if _, exists := reservedAliases[alias]; exists {
		return "", fmt.Errorf("host alias %q is a Mesh command; choose another alias", alias)
	}
	if _, err := session.ParseID(alias); err == nil {
		return "", fmt.Errorf("host alias %q looks like a session ID; choose another alias", alias)
	}
	return alias, nil
}

// LoadHosts reads and validates the local host address book.
func LoadHosts() ([]HostRecord, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read host config %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var config hostConfig
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("parse host config %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("parse host config %s: trailing data: %w", path, err)
	}
	if config.Version != hostConfigVersion {
		return nil, fmt.Errorf("parse host config %s: version %d is unsupported", path, config.Version)
	}
	aliases := make(map[string]string, len(config.Hosts))
	identities := make(map[string]string, len(config.Hosts))
	for i := range config.Hosts {
		host, err := validateHostRecord(config.Hosts[i])
		if err != nil {
			return nil, fmt.Errorf("parse host config %s: host %d: %w", path, i+1, err)
		}
		if prior := aliases[host.Alias]; prior != "" {
			return nil, fmt.Errorf("parse host config %s: alias %q is used by hosts %s and %s", path, host.Alias, prior, host.ID)
		}
		if prior := identities[host.ID]; prior != "" {
			return nil, fmt.Errorf("parse host config %s: host ID %s has aliases %q and %q", path, host.ID, prior, host.Alias)
		}
		aliases[host.Alias] = host.ID
		identities[host.ID] = host.Alias
		config.Hosts[i] = host
	}
	sortHosts(config.Hosts)
	return config.Hosts, nil
}

// SaveHost atomically adds or replaces one adopted host in the address book.
func SaveHost(record HostRecord) error {
	host, err := validateHostRecord(record)
	if err != nil {
		return err
	}
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	replaced := false
	for i, existing := range hosts {
		switch {
		case existing.ID == host.ID:
			hosts[i] = host
			replaced = true
		case existing.Alias == host.Alias:
			return fmt.Errorf("host alias %q already belongs to host %s", host.Alias, existing.ID)
		}
	}
	if !replaced {
		hosts = append(hosts, host)
	}
	sortHosts(hosts)
	return writeHostConfig(hostConfig{Version: hostConfigVersion, Hosts: hosts})
}

func validateHostRecord(record HostRecord) (HostRecord, error) {
	alias, err := ValidateHostAlias(record.Alias)
	if err != nil {
		return HostRecord{}, err
	}
	record.Alias = alias
	record.ID = strings.TrimSpace(record.ID)
	record.MeshIdentity = strings.TrimSpace(record.MeshIdentity)
	record.TailscaleName = strings.TrimSpace(record.TailscaleName)
	record.Endpoint = strings.TrimSpace(record.Endpoint)
	if record.ID == "" {
		return HostRecord{}, fmt.Errorf("host %q has no stable ID", alias)
	}
	if record.MeshIdentity == "" {
		return HostRecord{}, fmt.Errorf("host %q has no Mesh identity", alias)
	}
	endpoint, err := url.Parse(record.Endpoint)
	if err != nil || (endpoint.Scheme != "ws" && endpoint.Scheme != "wss") || endpoint.Host == "" || endpoint.Path == "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return HostRecord{}, fmt.Errorf("host %q has invalid WebSocket endpoint %q", alias, record.Endpoint)
	}
	record.Addresses = append([]string(nil), record.Addresses...)
	for i := range record.Addresses {
		record.Addresses[i] = strings.TrimSpace(record.Addresses[i])
		if _, err := netip.ParseAddr(record.Addresses[i]); err != nil {
			return HostRecord{}, fmt.Errorf("host %q has invalid Tailscale address %q: %w", alias, record.Addresses[i], err)
		}
	}
	return record, nil
}

func writeHostConfig(config hostConfig) error {
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
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure temporary host config: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary host config: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
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
		if hosts[i].Alias != hosts[j].Alias {
			return hosts[i].Alias < hosts[j].Alias
		}
		return hosts[i].ID < hosts[j].ID
	})
}

// ArgumentTarget is either a host alias or a syntactically valid session ID.
type ArgumentTarget struct {
	Host      *HostRecord
	SessionID string
}

// ResolveArgument classifies the root command's positional argument.
func ResolveArgument(value string, hosts []HostRecord) (ArgumentTarget, error) {
	var matched *HostRecord
	for i := range hosts {
		if strings.EqualFold(hosts[i].Alias, value) {
			host := hosts[i]
			matched = &host
			break
		}
	}
	id, idErr := session.ParseID(value)
	if matched != nil && idErr == nil {
		return ArgumentTarget{}, fmt.Errorf("%q is both host alias %q and session ID %s; rename the host alias", value, matched.Alias, id)
	}
	if matched != nil {
		return ArgumentTarget{Host: matched}, nil
	}
	if idErr == nil {
		return ArgumentTarget{SessionID: id}, nil
	}
	return ArgumentTarget{}, fmt.Errorf("%q is neither a known host alias nor a session ID", value)
}
