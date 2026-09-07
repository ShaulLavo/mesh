package updatenotice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const maximumCacheBytes = 2 << 20

var errLocked = errors.New("update notice refresh is already running")

func (s *Store) lock(ctx context.Context, name string, wait bool) (func(), error) {
	if s.directory == "" {
		return nil, errors.New("update notice cache directory is required")
	}
	if err := os.MkdirAll(s.directory, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(s.directory, name)
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := acquire(ctx, file, wait); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = file.Close() }, nil
}

func acquire(ctx context.Context, file *os.File, wait bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		if !wait {
			return errLocked
		}
		if err := pauseLock(ctx); err != nil {
			return err
		}
	}
}

func pauseLock(ctx context.Context) error {
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func readJSON(path string, target any) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close() //nolint:errcheck // read-only cache file
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maximumCacheBytes {
		return fmt.Errorf("invalid update notice cache %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumCacheBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maximumCacheBytes {
		return errors.New("update notice cache exceeds size limit")
	}
	return json.Unmarshal(data, target)
}

func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maximumCacheBytes {
		return errors.New("update notice cache exceeds size limit")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".notice-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name()) //nolint:errcheck // remove an unpublished temporary cache
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil { //nolint:gosec // configured local directory and a fixed or URL-hashed filename
		return err
	}
	directory, err := os.Open(filepath.Dir(path)) //nolint:gosec // sync the configured local cache directory
	if err != nil {
		return err
	}
	defer directory.Close() //nolint:errcheck // Sync reports persistence failures
	return directory.Sync()
}
