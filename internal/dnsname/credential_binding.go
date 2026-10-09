package dnsname

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type renewalCredentialBinding struct {
	Domain string `json:"domain"`
	ZoneID string `json:"zoneId"`
}

// A config path identifies the renewal job across primary-domain and credential
// changes. Keying by domain or zone ID would lose the previous pair on a change.
func renewalCredentialBindingPath(configPath, stateDir string) string {
	digest := sha256.Sum256([]byte(configPath))
	return filepath.Join(stateDir, "private-names", "zone-bindings", fmt.Sprintf("%x.json", digest))
}

func bindRenewalCredentials(configPath, stateDir string, config PrivateNamesConfig) error {
	return bindRenewalCredentialsWithSync(configPath, stateDir, config, (*os.File).Sync)
}

func bindRenewalCredentialsWithSync(configPath, stateDir string, config PrivateNamesConfig, syncFile func(*os.File) error) (result error) {
	path, err := filepath.Abs(renewalCredentialBindingPath(configPath, stateDir))
	if err != nil {
		return fmt.Errorf("dnsname: resolve renewal credential binding: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("dnsname: create renewal credential binding directory: %w", err)
	}
	// Freeze the physical directory before acquiring its stable lock. This also
	// permits state roots through directory symlinks such as macOS /var.
	directory, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("dnsname: resolve renewal credential binding directory: %w", err)
	}
	path = filepath.Join(directory, filepath.Base(path))
	// Keep this inode across replacements and failures. Unlinking a lock lets
	// processes waiting on the old inode race with holders of a new inode.
	lock, err := acquireCredentialBindingLock(path+".lock", (*os.File).Sync)
	if err != nil {
		return fmt.Errorf("dnsname: lock renewal credential binding: %w", err)
	}
	defer func() { result = errors.Join(result, lock.release()) }()

	desired := renewalCredentialBinding{Domain: config.Domain, ZoneID: config.ZoneID}
	encoded, err := json.Marshal(desired)
	if err != nil {
		return fmt.Errorf("dnsname: encode renewal credential binding: %w", err)
	}
	contents, err := readSecureFile(path, privateNamesConfigMaximum)
	if errors.Is(err, os.ErrNotExist) {
		// Preserve DNS-Write-only legacy tokens: the first pair comes from local
		// configuration, without a Zone Read API call or a new token scope.
		return publishCredentialBinding(path, encoded, syncFile)
	}
	if err != nil {
		return fmt.Errorf("dnsname: read renewal credential binding for %s: %w", configPath, err)
	}
	var previous renewalCredentialBinding
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&previous); err != nil {
		return fmt.Errorf("dnsname: parse renewal credential binding %s: %w", path, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return fmt.Errorf("dnsname: parse renewal credential binding %s: %w", path, err)
	}
	if previous.Domain == "" || previous.ZoneID == "" {
		return fmt.Errorf("dnsname: renewal credential binding %s is incomplete; restore it from the configuration backup", path)
	}
	if previous != desired {
		if config.ZoneDomain != config.Domain {
			return fmt.Errorf("dnsname: renewal credentials changed for %s; restore the previous domain and zone ID, or set zoneDomain to %q after checking the zone ID belongs to that domain", configPath, config.Domain)
		}
		return publishCredentialBinding(path, encoded, syncFile)
	}
	// A visible binding may have survived a failed sync or a previous process
	// crash. Readers must repair the same barrier before accepting that pair.
	if err := syncCredentialBindingPath(path, syncFile); err != nil {
		return err
	}
	return syncCredentialBindingAncestors(filepath.Dir(path), syncFile)
}

func publishCredentialBinding(path string, contents []byte, syncFile func(*os.File) error) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".binding-*")
	if err != nil {
		return fmt.Errorf("dnsname: create temporary credential binding: %w", err)
	}
	defer os.Remove(temporary.Name()) //nolint:errcheck // best-effort cleanup; published bindings keep their stable lock
	defer temporary.Close()           //nolint:errcheck // explicit close before publication is checked
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("dnsname: secure temporary credential binding: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		return fmt.Errorf("dnsname: write credential binding: %w", err)
	}
	if err := syncFile(temporary); err != nil {
		return fmt.Errorf("dnsname: sync credential binding: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("dnsname: close credential binding: %w", err)
	}
	// All readers and publishers hold the separate lock through ancestor syncs.
	if err := os.Rename(temporary.Name(), path); err != nil {
		return fmt.Errorf("dnsname: publish credential binding: %w", err)
	}
	return syncCredentialBindingAncestors(filepath.Dir(path), syncFile)
}

func syncCredentialBindingPath(path string, syncFile func(*os.File) error) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("dnsname: open credential binding durability barrier %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	syncErr := syncFile(file)
	if err := errors.Join(syncErr, file.Close()); err != nil {
		return fmt.Errorf("dnsname: sync credential binding durability barrier %s: %w", path, err)
	}
	return nil
}

func syncCredentialBindingAncestors(directory string, syncFile func(*os.File) error) error {
	// Sync the full physical chain on every successful initialization: after a
	// crash or failed fsync, MkdirAll cannot identify undurable visible ancestors.
	for {
		if err := syncCredentialBindingPath(directory, syncFile); err != nil {
			return err
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return nil
		}
		directory = parent
	}
}

// Use the same nonblocking advisory-lock protocol as renewal locks, with a
// bounded synchronous wait because runtime construction has no context input.
func acquireCredentialBindingLock(path string, syncFile func(*os.File) error) (*renewalLock, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	fd, err := unix.Open(path, flags, 0)
	if errors.Is(err, os.ErrNotExist) {
		if err := publishCredentialBindingLock(path, syncFile); err != nil {
			return nil, err
		}
		fd, err = unix.Open(path, flags, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("dnsname: open credential binding lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	closeOnError := func(cause error) (*renewalLock, error) { return nil, errors.Join(cause, file.Close()) }
	info, err := file.Stat()
	if err != nil {
		return closeOnError(fmt.Errorf("dnsname: inspect credential binding lock: %w", err))
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return closeOnError(errors.New("dnsname: credential binding lock must be a regular mode-0600 file"))
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &renewalLock{file: file}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return closeOnError(fmt.Errorf("dnsname: acquire credential binding lock: %w", err))
		}
		if !time.Now().Before(deadline) {
			return closeOnError(errors.New("dnsname: timed out waiting for credential binding lock"))
		}
		time.Sleep(renewalLockPollInterval)
	}
}

func publishCredentialBindingLock(path string, syncFile func(*os.File) error) error {
	// Prepare permissions before exposing the stable name, so concurrent openers
	// never observe an umask-restricted intermediate inode. Only the temporary
	// name is removed; a losing creator always opens the winner's stable inode.
	temporary, err := os.CreateTemp(filepath.Dir(path), ".binding-lock-*")
	if err != nil {
		return fmt.Errorf("dnsname: create temporary credential binding lock: %w", err)
	}
	defer os.Remove(temporary.Name()) //nolint:errcheck // stable lock is never removed
	defer temporary.Close()           //nolint:errcheck // explicit close is checked before publication
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("dnsname: secure temporary credential binding lock: %w", err)
	}
	if err := syncFile(temporary); err != nil {
		return fmt.Errorf("dnsname: sync temporary credential binding lock: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("dnsname: close temporary credential binding lock: %w", err)
	}
	if err := os.Link(temporary.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("dnsname: publish credential binding lock: %w", err)
	}
	return nil
}
