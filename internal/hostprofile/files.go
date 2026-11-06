package hostprofile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Resolve existing ancestors once, then walk from an anchored descriptor.
// The selected directory and newly created children cannot be symlinks.
func openDirectory(path string, create bool) (*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve profile directory: %w", err)
	}
	parent, err := canonicalParent(filepath.Dir(absolute))
	if err != nil {
		return nil, fmt.Errorf("resolve profile directory parent: %w", err)
	}
	absolute = filepath.Join(parent, filepath.Base(absolute))
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("anchor profile directory: %w", err)
	}
	dir := os.NewFile(uintptr(fd), "/")
	for _, part := range strings.Split(strings.TrimPrefix(absolute, "/"), "/") {
		if part == "" {
			continue
		}
		next, err := openDirectoryChild(dir, part, create)
		_ = dir.Close()
		if err != nil {
			return nil, err
		}
		dir = next
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &stat); err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("inspect profile directory: %w", err)
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0022 != 0 { //nolint:gosec // Unix effective UIDs are unsigned 32-bit values.
		_ = dir.Close()
		return nil, errors.New("profile directory must be owned by this user and not writable by group or others")
	}
	return dir, nil
}

func canonicalParent(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) || filepath.Dir(path) == path {
		return "", fmt.Errorf("resolve directory ancestor: %w", err)
	}
	parent, err := canonicalParent(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func openDirectoryChild(parent *os.File, name string, create bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(int(parent.Fd()), name, flags, 0)
	if errors.Is(err, unix.ENOENT) && create {
		return createDirectoryChild(parent, name, flags)
	}
	if err != nil {
		return nil, fmt.Errorf("open profile directory %s: %w", name, err)
	}
	return settleDirectoryChild(parent, fd, name, create)

}

func createDirectoryChild(parent *os.File, name string, flags int) (*os.File, error) {
	if err := unix.Mkdirat(int(parent.Fd()), name, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, fmt.Errorf("create profile directory: %w", err)
	}
	fd, err := unix.Openat(int(parent.Fd()), name, flags, 0)
	if err != nil {
		return nil, fmt.Errorf("open new profile directory: %w", err)
	}
	return settleDirectoryChild(parent, fd, name, true)
}

func settleDirectoryChild(parent *os.File, fd int, name string, write bool) (*os.File, error) {
	child := os.NewFile(uintptr(fd), name)
	if !write {
		return child, nil
	}
	// Existing directories can be visible after an earlier failed parent barrier.
	if err := parent.Sync(); err != nil {
		_ = child.Close()
		return nil, fmt.Errorf("settle profile directory parent: %w", err)
	}
	return child, nil
}

func openPrivateFile(dir *os.File, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fmt.Errorf("open profile file %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	if err := checkPrivateFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func checkPrivateFile(file *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fmt.Errorf("inspect profile file: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || stat.Mode&07777 != 0600 { //nolint:gosec // Unix effective UIDs are unsigned 32-bit values.
		return errors.New("profile files must be private 0600 regular files owned by this user with one link")
	}
	return nil
}

func readFile(dir *os.File) ([]byte, error) {
	file, err := openPrivateFile(dir, "profile.json", unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, int64(HostLimits.Encoded)+1))
	if err != nil {
		return nil, fmt.Errorf("read profile.json: %w", err)
	}
	return data, nil
}

func acquireLock(ctx context.Context, dir *os.File) (*os.File, error) {
	file, err := openPrivateFile(dir, ".profile.lock", unix.O_RDWR|unix.O_CREAT|unix.O_EXCL)
	if errors.Is(err, unix.EEXIST) {
		file, err = openPrivateFile(dir, ".profile.lock", unix.O_RDWR)
	}
	if err != nil {
		return nil, err
	}
	if err := waitLock(ctx, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func waitLock(ctx context.Context, file *os.File) error {
	wait, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := wait.Err(); err != nil {
			return fmt.Errorf("acquire profile writer lock: %w", err)
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("lock profile: %w", err)
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("acquire profile writer lock: %w", wait.Err())
		case <-ticker.C:
		}
	}
}

func temporaryFile(dir *os.File) (*os.File, string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, "", fmt.Errorf("profile temporary name: %w", err)
	}
	name := ".profile-" + hex.EncodeToString(random[:]) + ".json"
	file, err := openPrivateFile(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	return file, name, err
}

func writeAll(file *os.File, data []byte) error {
	n, err := file.Write(data)
	if err != nil {
		return fmt.Errorf("write profile: %w", err)
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func syncFile(file *os.File) error {
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync profile file: %w", err)
	}
	return nil
}
func renameProfile(dir *os.File, name string) error {
	if err := unix.Renameat(int(dir.Fd()), name, int(dir.Fd()), "profile.json"); err != nil {
		return fmt.Errorf("publish profile: %w", err)
	}
	return nil
}

// EditFile keeps editor reads anchored even if an editor replaces the inode.
// Failed edits retain their private file for an explicit human repair.
type EditFile struct {
	dir        *os.File
	path, name string
}

func NewEditFile(stateDir string, data []byte) (*EditFile, error) {
	dir, err := openDirectory(stateDir, false)
	if err != nil {
		return nil, err
	}
	file, name, err := temporaryFile(dir)
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	err = writeAll(file, data)
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = unix.Unlinkat(int(dir.Fd()), name, 0)
		_ = dir.Close()
		return nil, fmt.Errorf("write editable profile: %w", err)
	}
	return &EditFile{dir: dir, path: filepath.Join(stateDir, name), name: name}, nil
}
func (f *EditFile) Path() string { return f.path }
func (f *EditFile) Close()       { _ = f.dir.Close() }
func (f *EditFile) Remove() error {
	if err := unix.Unlinkat(int(f.dir.Fd()), f.name, 0); err != nil {
		return fmt.Errorf("remove editable profile: %w", err)
	}
	return nil
}
func (f *EditFile) Read() ([]byte, error) {
	file, err := openPrivateFile(f.dir, f.name, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, int64(HostLimits.Encoded)+1))
	if err != nil {
		return nil, fmt.Errorf("read editable profile: %w", err)
	}
	return data, nil
}
