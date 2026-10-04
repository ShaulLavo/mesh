package identity

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// LoadOwned reads an existing identity through a bounded, owned file descriptor.
func LoadOwned(stateDir string) (Host, error) {
	if stateDir == "" {
		return Host{}, errors.New("existing identity state directory is empty")
	}
	path := filepath.Join(stateDir, privateKeyName)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // explicit local identity path; held descriptor rejects symlinks and special files
	if err != nil {
		return Host{}, fmt.Errorf("open existing identity: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return Host{}, fmt.Errorf("inspect existing identity: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || int64(stat.Uid) != int64(os.Geteuid()) {
		return Host{}, errors.New("existing identity must be an owned regular file with permissions 0600")
	}
	const maximumIdentitySize = 16 << 10
	contents, err := io.ReadAll(io.LimitReader(file, maximumIdentitySize+1))
	if err != nil {
		return Host{}, fmt.Errorf("read existing identity: %w", err)
	}
	if len(contents) > maximumIdentitySize {
		return Host{}, errors.New("existing identity exceeds size limit")
	}
	private, err := parsePrivateKey(contents, path)
	if err != nil {
		return Host{}, err
	}
	return hostFor(private), nil
}
