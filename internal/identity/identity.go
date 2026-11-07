// Package identity manages the stable cryptographic identity of one Mesh host.
package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

const privateKeyName = "identity.key"

// Host identifies one Mesh host independently of its address or display name.
type Host struct {
	ID        string
	PublicKey ed25519.PublicKey
}

// Load reads the existing host identity without creating one.
func Load(stateDir string) (Host, error) {
	host, _, err := LoadPrivate(stateDir)
	return host, err
}

// LoadPrivate reads the existing identity and signing key without creating one.
func LoadPrivate(stateDir string) (Host, ed25519.PrivateKey, error) {
	if stateDir == "" {
		return Host{}, nil, errors.New("identity state directory is empty")
	}
	private, err := loadPrivateKey(filepath.Join(stateDir, privateKeyName))
	if err != nil {
		return Host{}, nil, err
	}
	return hostFor(private), private, nil
}

// LoadOrCreate loads the host identity from stateDir or creates it atomically.
func LoadOrCreate(stateDir string) (Host, ed25519.PrivateKey, error) {
	if stateDir == "" {
		return Host{}, nil, errors.New("identity state directory is empty")
	}

	keyPath := filepath.Join(stateDir, privateKeyName)
	private, err := loadPrivateKey(keyPath)
	if err == nil {
		return hostFor(private), private, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Host{}, nil, err
	}

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return Host{}, nil, fmt.Errorf("create identity state directory %s: %w", stateDir, err)
	}

	_, candidate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Host{}, nil, fmt.Errorf("generate identity key: %w", err)
	}
	return publishPrivateKey(stateDir, keyPath, candidate)
}

// LoadCredential parses the bytes from one checked, bounded no-follow descriptor.
// It never creates state or consults the machine's ordinary identity directory.
func LoadCredential(directory string) (Host, ed25519.PrivateKey, error) {
	directoryFD, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return Host{}, nil, fmt.Errorf("identity: open credential directory: %w", err)
	}
	dir := os.NewFile(uintptr(directoryFD), directory)
	defer func() { _ = dir.Close() }()
	info, err := dir.Stat()
	if err != nil {
		return Host{}, nil, fmt.Errorf("identity: inspect credential directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Geteuid()) || info.Mode().Perm() != 0700 {
		return Host{}, nil, errors.New("identity: credential directory must be owned with permissions 0700")
	}
	fd, err := unix.Openat(int(dir.Fd()), privateKeyName, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return Host{}, nil, fmt.Errorf("identity: open credential: %w", err)
	}
	file := os.NewFile(uintptr(fd), privateKeyName)
	defer func() { _ = file.Close() }()
	info, err = file.Stat()
	if err != nil {
		return Host{}, nil, fmt.Errorf("identity: inspect credential: %w", err)
	}
	stat, ok = info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Nlink != 1 || int64(stat.Uid) != int64(os.Geteuid()) {
		return Host{}, nil, errors.New("identity: credential must be an owned 0600 regular file with one link")
	}
	data, err := io.ReadAll(io.LimitReader(file, (16<<10)+1))
	if err != nil {
		return Host{}, nil, fmt.Errorf("identity: read credential: %w", err)
	}
	if len(data) > 16<<10 {
		return Host{}, nil, errors.New("identity: credential exceeds size limit")
	}
	private, err := parsePrivateKey(data, privateKeyName)
	if err != nil {
		return Host{}, nil, err
	}
	return hostFor(private), private, nil
}

func CreateCredential(directory string) (Host, error) {
	if directory == "" {
		return Host{}, errors.New("identity: explicit credential directory required")
	}
	host, _, err := LoadCredential(directory)
	if err == nil {
		return host, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Host{}, err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return Host{}, fmt.Errorf("identity: create credential directory: %w", err)
	}
	// Validate the selected directory even when the key is absent.
	_, _, err = LoadCredential(directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Host{}, err
	}
	_, candidate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Host{}, fmt.Errorf("identity: generate credential: %w", err)
	}
	_, _, err = publishPrivateKeyWithLoader(directory, filepath.Join(directory, privateKeyName), candidate, func(string) (ed25519.PrivateKey, error) { _, key, err := LoadCredential(directory); return key, err })
	if err != nil {
		return Host{}, err
	}
	host, _, err = LoadCredential(directory)
	return host, err
}

func publishPrivateKey(stateDir, keyPath string, candidate ed25519.PrivateKey) (Host, ed25519.PrivateKey, error) {
	return publishPrivateKeyWithLoader(stateDir, keyPath, candidate, loadPrivateKey)
}
func publishPrivateKeyWithLoader(stateDir, keyPath string, candidate ed25519.PrivateKey, load func(string) (ed25519.PrivateKey, error)) (Host, ed25519.PrivateKey, error) {
	// OpenSSH format, not PKCS#8: the T15 front door hands this exact file to a
	// stock ssh client with -i, and OpenSSH before 10.0 cannot read a PKCS#8
	// Ed25519 key.
	block, err := ssh.MarshalPrivateKey(candidate, "")
	if err != nil {
		return Host{}, nil, fmt.Errorf("encode identity key: %w", err)
	}
	contents := pem.EncodeToMemory(block)

	temporary, err := os.CreateTemp(stateDir, ".identity-key-*")
	if err != nil {
		return Host{}, nil, fmt.Errorf("create temporary identity key: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath) //nolint:errcheck // best-effort cleanup after atomic publication

	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return Host{}, nil, fmt.Errorf("write temporary identity key: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return Host{}, nil, fmt.Errorf("sync temporary identity key: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return Host{}, nil, fmt.Errorf("close temporary identity key: %w", err)
	}

	// Link publishes a complete file without replacing an identity created by
	// another process between the initial read and this point.
	if err := os.Link(temporaryPath, keyPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			private, loadErr := load(keyPath)
			if loadErr != nil {
				return Host{}, nil, loadErr
			}
			return hostFor(private), private, nil
		}
		return Host{}, nil, fmt.Errorf("publish identity key %s: %w", keyPath, err)
	}

	return hostFor(candidate), candidate, nil
}

func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat identity key %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("identity key %s is not a regular file", path)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		return nil, fmt.Errorf("identity key %s has permissions %04o, want 0600", path, permissions)
	}

	contents, err := os.ReadFile(path) //nolint:gosec // path is the fixed identity filename under the caller-selected local state directory
	if err != nil {
		return nil, fmt.Errorf("read identity key %s: %w", path, err)
	}
	return parsePrivateKey(contents, path)
}

func parsePrivateKey(contents []byte, path string) (ed25519.PrivateKey, error) {
	block, rest := pem.Decode(contents)
	if block == nil || block.Type != "OPENSSH PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("parse identity key %s: not an OpenSSH private key", path)
	}
	parsed, err := ssh.ParseRawPrivateKey(contents)
	if err != nil {
		return nil, fmt.Errorf("parse identity key %s: %w", path, err)
	}
	private, ok := parsed.(*ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("parse identity key %s: key is not Ed25519", path)
	}
	return *private, nil
}

func hostFor(private ed25519.PrivateKey) Host {
	public := private.Public().(ed25519.PublicKey)
	public = bytes.Clone(public)
	return Host{
		ID:        base64.RawURLEncoding.EncodeToString(public),
		PublicKey: public,
	}
}
