// Package identity manages the stable cryptographic identity of one Mesh host.
package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const privateKeyName = "identity.key"

// Host identifies one Mesh host independently of its address or display name.
type Host struct {
	ID        string
	PublicKey ed25519.PublicKey
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

func publishPrivateKey(stateDir, keyPath string, candidate ed25519.PrivateKey) (Host, ed25519.PrivateKey, error) {
	encoded, err := x509.MarshalPKCS8PrivateKey(candidate)
	if err != nil {
		return Host{}, nil, fmt.Errorf("encode identity key: %w", err)
	}
	contents := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})

	temporary, err := os.CreateTemp(stateDir, ".identity-key-*")
	if err != nil {
		return Host{}, nil, fmt.Errorf("create temporary identity key: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return Host{}, nil, fmt.Errorf("write temporary identity key: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return Host{}, nil, fmt.Errorf("sync temporary identity key: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return Host{}, nil, fmt.Errorf("close temporary identity key: %w", err)
	}

	// Link publishes a complete file without replacing an identity created by
	// another process between the initial read and this point.
	if err := os.Link(temporaryPath, keyPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			private, loadErr := loadPrivateKey(keyPath)
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

	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read identity key %s: %w", path, err)
	}
	block, rest := pem.Decode(contents)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("parse identity key %s: invalid PKCS#8 PEM", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse identity key %s: %w", path, err)
	}
	private, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("parse identity key %s: key is not Ed25519", path)
	}
	return private, nil
}

func hostFor(private ed25519.PrivateKey) Host {
	public := private.Public().(ed25519.PublicKey)
	public = bytes.Clone(public)
	return Host{
		ID:        base64.RawURLEncoding.EncodeToString(public),
		PublicKey: public,
	}
}
