package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

const AuthorizedKeysMaximum = 1 << 20

type DeviceGrant struct {
	Identity    string
	Key         ssh.PublicKey
	incarnation [sha256.Size]byte
}

func currentGrant(path string, presented ssh.PublicKey, incarnation *[sha256.Size]byte) (DeviceGrant, bool) {
	// Writers publish whole policies atomically. Admission reads the published inode,
	// so an unrelated writer lock cannot impersonate revocation or stall a handshake.
	grants, err := DeviceGrants(path)
	if err != nil {
		return DeviceGrant{}, false
	}
	for _, grant := range grants {
		if !bytes.Equal(grant.Key.Marshal(), presented.Marshal()) {
			continue
		}
		if incarnation == nil || grant.incarnation == *incarnation {
			return grant, true
		}
	}
	return DeviceGrant{}, false
}

func Granted(path string, presented ssh.PublicKey) bool {
	_, ok := currentGrant(path, presented, nil)
	return ok
}

// BindGrant captures the approved line's incarnation from a published snapshot.
// Reapproval creates a new line identity, so a missed removal cannot heal a socket.
func BindGrant(path string, presented ssh.PublicKey) (func() bool, bool) {
	grant, ok := currentGrant(path, presented, nil)
	if !ok {
		return nil, false
	}
	return func() bool {
		_, current := currentGrant(path, presented, &grant.incarnation)
		return current
	}, true
}

func BindIdentity(stateDir, id string) (func() bool, bool) {
	public, err := IdentityKey(id)
	if err != nil {
		return nil, false
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		return nil, false
	}
	return BindGrant(filepath.Join(stateDir, "authorized_keys"), key)
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
	return parseDeviceGrants(contents)
}

func parseDeviceGrants(contents []byte) ([]DeviceGrant, error) {
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
		grant := DeviceGrant{Key: key, incarnation: sha256.Sum256(trimmed)}
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
	lock, err := os.OpenFile(filepath.Join(stateDir, "device-grants.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600) //nolint:gosec // fixed policy lock; reject symlinks and special files before locking
	if err != nil {
		return fmt.Errorf("identity: open grants lock: %w", err)
	}
	defer lock.Close() //nolint:errcheck // closing releases exclusive policy ownership
	info, err := lock.Stat()
	if err != nil {
		return fmt.Errorf("identity: inspect grants lock: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return errors.New("identity: grants lock must be a regular file with safe permissions")
	}
	if err := lockGrantWriter(lock); err != nil {
		return err
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

// Only mutations contend. Published-snapshot readers never wait for this lock.
func lockGrantWriter(file *os.File) error {
	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return fmt.Errorf("identity: lock grants: %w", err)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("identity: grants writer contention exceeded 250ms: %w", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func existingGrant(contents []byte, key ssh.PublicKey) (bool, error) {
	grants, err := parseDeviceGrants(contents)
	if err != nil {
		return false, err
	}
	for _, grant := range grants {
		if bytes.Equal(grant.Key.Marshal(), key.Marshal()) {
			return true, nil
		}
	}
	return false, nil
}

func changedGrants(contents []byte, key ssh.PublicKey, approve bool) ([]byte, error) {
	if approve {
		exists, err := existingGrant(contents, key)
		if err != nil {
			return nil, err
		}
		if exists {
			return contents, nil
		}
	}
	next, err := removeGrants(contents, key)
	if err != nil {
		return nil, err
	}
	if approve {
		next, err = appendGrant(next, key)
		if err != nil {
			return nil, err
		}
	}
	if len(next) > AuthorizedKeysMaximum {
		return nil, errors.New("identity: device grants exceed size limit")
	}
	return next, nil
}

func removeGrants(contents []byte, key ssh.PublicKey) ([]byte, error) {
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
	return next, nil
}

func appendGrant(contents []byte, key ssh.PublicKey) ([]byte, error) {
	if len(contents) > 0 && contents[len(contents)-1] != '\n' {
		contents = append(contents, '\n')
	}
	var incarnation [16]byte
	if _, err := rand.Read(incarnation[:]); err != nil {
		return nil, fmt.Errorf("identity: create grant incarnation: %w", err)
	}
	line := bytes.TrimSuffix(ssh.MarshalAuthorizedKey(key), []byte("\n"))
	contents = append(contents, line...)
	contents = append(contents, []byte(" mesh-grant:"+base64.RawURLEncoding.EncodeToString(incarnation[:])+"\n")...)
	return contents, nil
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
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // fixed policy path; reject symlinks and nonregular descriptors
	if err != nil {
		return nil, fmt.Errorf("identity: open authorized_keys %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // a read-only authentication attempt has no close result to preserve
	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("identity: inspect opened authorized_keys %s: %w", path, err)
	}
	if !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("identity: authorized_keys %s is not a regular file", path)
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
