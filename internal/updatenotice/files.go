package updatenotice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shaul/mesh/internal/hostprofile"
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
	directoryFD, err := unix.Open(s.directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open notice lock directory: %w", err)
	}
	directory := os.NewFile(uintptr(directoryFD), s.directory)
	defer func() { _ = directory.Close() }()
	if err := privateNoticeFile(directory, true); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(directoryFD, name, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("open notice state lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if err := privateNoticeFile(file, false); err != nil {
		_ = file.Close()
		return nil, err
	}

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
	if err := privateNoticeFile(file, false); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maximumCacheBytes {
		return fmt.Errorf("invalid update notice cache %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumCacheBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maximumCacheBytes {
		return errors.New("update notice cache exceeds size limit")
	}
	if err := hostprofile.CheckJSON(json.NewDecoder(bytes.NewReader(data)), 0, 32); err != nil {
		return fmt.Errorf("check update cache JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode update cache: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("trailing update notice data")
	}
	return nil
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
	return settleCache(filepath.Dir(path))
}
func settleCache(directory string) error {
	if err := syncNoticeDirectory(directory); err != nil {
		return err
	}
	physical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return fmt.Errorf("resolve cache publication directory: %w", err)
	}
	for {
		parent := filepath.Dir(physical)
		if parent == physical {
			return nil
		}
		if err := syncNoticeDirectory(parent); err != nil {
			return err
		}
		physical = parent
	}
}

var syncNoticeDirectory = func(directory string) error {
	fd, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open cache publication directory: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("settle cache publication: %w", err)
	}
	return nil
}

func privateNoticeFile(file *os.File, directory bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fmt.Errorf("inspect notice state: %w", err)
	}
	kind := uint32(unix.S_IFREG)
	if directory {
		kind = unix.S_IFDIR
	}
	forbidden := uint32(0077)
	if directory {
		forbidden = 0022
	}
	if uint32(stat.Mode)&unix.S_IFMT != kind || uint32(stat.Mode)&forbidden != 0 || int64(stat.Uid) != int64(os.Geteuid()) || !directory && stat.Nlink != 1 {
		return errors.New("notice state must be private, owned and regular")
	}
	return nil
}
