package appcaller

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrIdentityExists reports a name that already has a credential. A caller
// needing a new identity picks a new name; an existing one is never replaced.
var ErrIdentityExists = errors.New("application identity already exists")

// ErrIdentityNotFound reports a name without a credential file.
var ErrIdentityNotFound = errors.New("application identity not found")

// Identity is one configured caller on the client device.
type Identity struct {
	Name       string
	Credential Credential
	CreatedAt  time.Time
}

// Summary describes an identity without its secret.
type Summary struct {
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"createdAt"`
	Path        string    `json:"path"`
}

type identityFile struct {
	Name       string    `json:"name"`
	Credential string    `json:"credential"`
	CreatedAt  time.Time `json:"createdAt"`
}

const identityExtension = ".json"

// Dir is where identities live under the Mesh config directory.
func Dir(configDir string) string { return filepath.Join(configDir, "mcp", "identities") }

// ValidName accepts short names that are safe as file names on every host.
func ValidName(name string) error {
	if len(name) < 1 || len(name) > 64 {
		return fmt.Errorf("identity name must be 1 to 64 characters")
	}
	for i, ch := range name {
		letter := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'
		if letter || (i > 0 && (ch == '-' || ch == '_' || ch == '.')) {
			continue
		}
		return fmt.Errorf("identity name %q may use letters, digits, '-', '_' and '.', starting with a letter or digit", name)
	}
	return nil
}

func identityPath(configDir, name string) string {
	return filepath.Join(Dir(configDir), name+identityExtension)
}

// Create generates and stores a new identity. It never overwrites one.
func Create(configDir, name string, now time.Time) (Identity, error) {
	if err := ValidName(name); err != nil {
		return Identity{}, err
	}
	credential, err := Generate()
	if err != nil {
		return Identity{}, err
	}
	identity := Identity{Name: name, Credential: credential, CreatedAt: now.UTC().Truncate(time.Second)}
	data, err := json.MarshalIndent(identityFile{Name: name, Credential: credential.Encode(), CreatedAt: identity.CreatedAt}, "", "  ")
	if err != nil {
		return Identity{}, fmt.Errorf("encode identity %s: %w", name, err)
	}
	if err := writeExclusive(Dir(configDir), name+identityExtension, append(data, '\n')); err != nil {
		return Identity{}, fmt.Errorf("store identity %s: %w", name, err)
	}
	return identity, nil
}

// Load reads one identity and checks that its file is private.
func Load(configDir, name string) (Identity, error) {
	if err := ValidName(name); err != nil {
		return Identity{}, err
	}
	path := identityPath(configDir, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Identity{}, fmt.Errorf("identity %s: %w", name, ErrIdentityNotFound)
	}
	if err != nil {
		return Identity{}, fmt.Errorf("identity %s: %w", name, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return Identity{}, fmt.Errorf("identity %s: %s must be a private regular file (mode 0600)", name, path)
	}
	// #nosec G304 -- The path is built from the validated identity name under the config directory.
	data, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, fmt.Errorf("identity %s: %w", name, err)
	}
	var file identityFile
	if err := json.Unmarshal(data, &file); err != nil || file.Name != name {
		return Identity{}, fmt.Errorf("identity %s: damaged file %s", name, path)
	}
	credential, err := ParseCredential(file.Credential)
	if err != nil {
		return Identity{}, fmt.Errorf("identity %s: %w", name, err)
	}
	return Identity{Name: name, Credential: credential, CreatedAt: file.CreatedAt}, nil
}

// List returns every readable identity, sorted by name. Damaged entries are
// skipped so one bad file does not hide the others.
func List(configDir string) ([]Summary, error) {
	entries, err := os.ReadDir(Dir(configDir))
	if errors.Is(err, os.ErrNotExist) {
		return []Summary{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list identities: %w", err)
	}
	out := make([]Summary, 0, len(entries))
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), identityExtension)
		if !ok || ValidName(name) != nil {
			continue
		}
		identity, err := Load(configDir, name)
		if err != nil {
			continue
		}
		out = append(out, Summary{Name: name, Fingerprint: identity.Credential.Fingerprint(), CreatedAt: identity.CreatedAt, Path: identityPath(configDir, name)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// writeExclusive publishes a complete private file under name, failing if the
// name exists. The hard link is the exclusive step, so a concurrent create of
// the same name cannot replace a credential another frontend already uses.
func writeExclusive(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create identity directory %s: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, ".identity-*")
	if err != nil {
		return fmt.Errorf("create identity file in %s: %w", dir, err)
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("make identity file private: %w", err)
	}
	_, err = temp.Write(data)
	err = errors.Join(err, temp.Sync(), temp.Close())
	if err != nil {
		return fmt.Errorf("write identity file: %w", err)
	}
	if err := os.Link(temp.Name(), filepath.Join(dir, name)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrIdentityExists
		}
		return fmt.Errorf("publish identity file %s: %w", name, err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	// #nosec G304 -- dir is the identity directory under the config directory.
	handle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open identity directory %s: %w", dir, err)
	}
	return errors.Join(handle.Sync(), handle.Close())
}
