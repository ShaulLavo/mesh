package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/crypto/ssh"
)

const AuthorizedKeysMaximum = 1 << 20

type DeviceGrant struct {
	Identity string
	Key      ssh.PublicKey
}

func Granted(path string, presented ssh.PublicKey) bool {
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(path), "device-grants.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer lock.Close() //nolint:errcheck // closing releases the shared admission lock
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_SH) != nil {
		return false
	}
	grants, err := DeviceGrants(path)
	if err != nil {
		return false
	}
	for _, grant := range grants {
		if bytes.Equal(grant.Key.Marshal(), presented.Marshal()) {
			return true
		}
	}
	return false
}

func GrantedIdentity(stateDir, id string) bool {
	public, err := IdentityKey(id)
	if err != nil {
		return false
	}
	key, err := ssh.NewPublicKey(public)
	return err == nil && Granted(filepath.Join(stateDir, "authorized_keys"), key)
}

func IdentityKey(id string) (ed25519.PublicKey, error) {
	key, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || len(key) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(key) != id {
		return nil, errors.New("identity: invalid Ed25519 device identity")
	}
	return ed25519.PublicKey(key), nil
}

func DeviceGrants(path string) ([]DeviceGrant, error) {
	contents, err := ReadAuthorizedKeys(path)
	if err != nil {
		return nil, err
	}
	var grants []DeviceGrant
	for _, line := range bytes.SplitAfter(contents, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || trimmed[0] == '#' {
			continue
		}
		key, _, options, _, err := ssh.ParseAuthorizedKey(line)
		if err != nil {
			return nil, fmt.Errorf("identity: parse device grants: %w", err)
		}
		if len(options) != 0 {
			continue
		}
		grant := DeviceGrant{Key: key}
		if cryptoKey, ok := key.(ssh.CryptoPublicKey); ok {
			if public, ok := cryptoKey.CryptoPublicKey().(ed25519.PublicKey); ok {
				grant.Identity = base64.RawURLEncoding.EncodeToString(public)
			}
		}
		grants = append(grants, grant)
	}
	return grants, nil
}

func ApproveDevice(stateDir, id string) error { return changeDevice(stateDir, id, true) }
func RevokeDevice(stateDir, id string) error  { return changeDevice(stateDir, id, false) }

func changeDevice(stateDir, id string, approve bool) error {
	public, err := IdentityKey(id)
	if err != nil {
		return err
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		return fmt.Errorf("identity: encode device key: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(stateDir, "device-grants.lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // fixed owner-controlled state-directory lock
	if err != nil {
		return fmt.Errorf("identity: open grants lock: %w", err)
	}
	defer lock.Close() //nolint:errcheck // the lock has no buffered writes
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("identity: lock grants: %w", err)
	}
	path := filepath.Join(stateDir, "authorized_keys")
	contents, err := ReadAuthorizedKeys(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	next, err := changedGrants(contents, key, approve)
	if err != nil {
		return err
	}
	return publishGrants(stateDir, path, next)
}

func changedGrants(contents []byte, key ssh.PublicKey, approve bool) ([]byte, error) {
	var next []byte
	for _, line := range bytes.SplitAfter(contents, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || trimmed[0] == '#' {
			next = append(next, line...)
			continue
		}
		existing, _, _, _, err := ssh.ParseAuthorizedKey(line)
		if err != nil {
			return nil, fmt.Errorf("identity: parse device key for mutation: %w", err)
		}
		if !bytes.Equal(existing.Marshal(), key.Marshal()) {
			next = append(next, line...)
		}
	}
	if approve {
		if len(next) > 0 && next[len(next)-1] != '\n' {
			next = append(next, '\n')
		}
		next = append(next, ssh.MarshalAuthorizedKey(key)...)
	}
	if len(next) > AuthorizedKeysMaximum {
		return nil, errors.New("identity: device grants exceed size limit")
	}
	return next, nil
}

func publishGrants(stateDir, path string, next []byte) error {
	file, err := os.CreateTemp(stateDir, ".device-grants-*")
	if err != nil {
		return fmt.Errorf("identity: stage grants: %w", err)
	}
	defer os.Remove(file.Name()) //nolint:errcheck // cleanup after atomic publication
	if _, err := file.Write(next); err != nil {
		_ = file.Close()
		return fmt.Errorf("identity: write grants: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("identity: sync grants: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("identity: close grants: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("identity: publish grants: %w", err)
	}
	directory, err := os.Open(stateDir) //nolint:gosec // fixed daemon state directory, never a remote path
	if err != nil {
		return fmt.Errorf("identity: open grants directory: %w", err)
	}
	defer directory.Close() //nolint:errcheck // directory contains no buffered writes
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("identity: sync grants directory: %w", err)
	}
	return nil
}

func ReadAuthorizedKeys(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("identity: inspect authorized_keys %s: %w", path, err)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("identity: authorized_keys %s is not a regular file", path)
	}
	file, err := os.Open(path) //nolint:gosec // the daemon supplies its fixed state-directory authorized_keys path
	if err != nil {
		return nil, fmt.Errorf("identity: open authorized_keys %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // a read-only authentication attempt has no close result to preserve
	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("identity: inspect opened authorized_keys %s: %w", path, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("identity: authorized_keys %s changed while opening", path)
	}
	permissions := opened.Mode().Perm()
	if permissions&0o022 != 0 {
		return nil, fmt.Errorf("identity: authorized_keys %s has unsafe permissions %04o", path, permissions)
	}
	if permissions&0o444 == 0 {
		return nil, fmt.Errorf("identity: authorized_keys %s is not readable", path)
	}
	contents, err := io.ReadAll(io.LimitReader(file, AuthorizedKeysMaximum+1))
	if err != nil {
		return nil, fmt.Errorf("identity: read authorized_keys %s: %w", path, err)
	}
	if len(contents) > AuthorizedKeysMaximum {
		return nil, fmt.Errorf("identity: authorized_keys %s exceeds %d bytes", path, AuthorizedKeysMaximum)
	}
	return contents, nil
}
