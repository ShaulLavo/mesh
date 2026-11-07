package skills

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

type Problem struct {
	Kind string
	Err  error
}

func (p *Problem) Error() string    { return p.Err.Error() }
func (p *Problem) Unwrap() error    { return p.Err }
func unsafe(err error) error        { return &Problem{Kind: "unsafe", Err: err} }
func conflict(message string) error { return &Problem{Kind: "conflict", Err: errors.New(message)} }

func canonicalRoot(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", unsafe(fmt.Errorf("resolve skills root: %w", err))
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", unsafe(fmt.Errorf("resolve skills root: %w", err))
	}
	return resolved, nil
}
func openDirectory(name string, create bool) (*os.File, error) {
	if !filepath.IsAbs(name) {
		return nil, unsafe(errors.New("skill directory must be absolute"))
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("anchor skill directory: %w", err)
	}
	dir := os.NewFile(uintptr(fd), "/")
	for _, part := range strings.Split(strings.TrimPrefix(name, "/"), "/") {
		if part == "" {
			continue
		}
		next, err := childDirectory(dir, part, create)
		_ = dir.Close()
		if err != nil {
			return nil, err
		}
		dir = next
	}
	if err := checkDirectory(dir); err != nil {
		_ = dir.Close()
		return nil, err
	}
	return dir, nil
}
func childDirectory(parent *os.File, name string, create bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(int(parent.Fd()), name, flags, 0)
	if errors.Is(err, unix.ENOENT) && create {
		if err := unix.Mkdirat(int(parent.Fd()), name, 0755); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("create skill directory: %w", err)
		}
		fd, err = unix.Openat(int(parent.Fd()), name, flags, 0)
	}
	if err != nil {
		return nil, pathError("open directory", name, err)
	}
	dir := os.NewFile(uintptr(fd), name)
	if !create {
		return dir, nil
	}
	if err := parent.Sync(); err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("sync skill directory parent: %w", err)
	}
	return dir, nil
}
func checkDirectory(dir *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &stat); err != nil {
		return fmt.Errorf("inspect skill directory: %w", err)
	}
	if int(stat.Uid) != os.Geteuid() || stat.Mode&0022 != 0 {
		return unsafe(errors.New("skill directory must be owned by this user and not writable by group or others"))
	}
	return nil
}
func fileParent(root *os.File, name string, create bool) (*os.File, string, error) {
	if !validPath(name) {
		return nil, "", unsafe(fmt.Errorf("invalid skill path %q", name))
	}
	fd, err := unix.Dup(int(root.Fd()))
	if err != nil {
		return nil, "", fmt.Errorf("duplicate skill directory: %w", err)
	}
	dir := os.NewFile(uintptr(fd), root.Name())
	parts := strings.Split(name, "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := childDirectory(dir, part, create)
		_ = dir.Close()
		if err != nil {
			return nil, "", err
		}
		if err := checkDirectory(next); err != nil {
			_ = next.Close()
			return nil, "", err
		}
		dir = next
	}
	return dir, parts[len(parts)-1], nil
}
func openFile(dir *os.File, name string, flags int, mode uint32) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, mode)
	if err != nil {
		return nil, pathError("open file", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect skill file: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || int(stat.Uid) != os.Geteuid() || stat.Mode&0022 != 0 {
		_ = file.Close()
		return nil, unsafe(fmt.Errorf("skill file %s must be a regular owned file with one link and no group or other write access", name))
	}
	return file, nil
}
func pathError(action, name string, err error) error {
	wrapped := fmt.Errorf("%s %s: %w", action, name, err)
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return unsafe(wrapped)
	}
	return wrapped
}

var errContentTooLarge = errors.New("skill file exceeds its size limit")

func readAt(root *os.File, name string, limit int) ([]byte, error) {
	dir, leaf, err := metadataParent(root, name, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	file, err := openFile(dir, leaf, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("read skill file: %w", err)
	}
	if len(data) > limit {
		return nil, fmt.Errorf("skill file %s: %w", name, errContentTooLarge)
	}
	return data, nil
}
func metadataParent(root *os.File, name string, create bool) (*os.File, string, error) {
	if name == manifestName {
		dir, err := duplicateDirectory(root)
		return dir, name, err
	}
	return fileParent(root, name, create)
}
func duplicateDirectory(root *os.File) (*os.File, error) {
	// Opening '.' keeps close ownership separate from the caller's anchored descriptor.
	fd, err := unix.Openat(int(root.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("duplicate skill descriptor: %w", err)
	}
	return os.NewFile(uintptr(fd), root.Name()), nil
}
func temporaryName() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("skill temporary name: %w", err)
	}
	return ".mesh-skill-" + hex.EncodeToString(bytes[:]), nil
}
func (m *Manager) publish(root *os.File, name string, data []byte, mode uint32) error {
	dir, leaf, err := metadataParent(root, name, true)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if dir == nil {
		return errors.New("cannot anchor skill publication")
	}
	if err := checkExisting(dir, leaf); err != nil {
		return err
	}
	temp, err := temporaryName()
	if err != nil {
		return err
	}
	file, err := openFile(dir, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(int(dir.Fd()), temp, 0) }()
	err = writeFile(file, data)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return fmt.Errorf("close skill file: %w", closeErr)
	}
	if err := unix.Renameat(int(dir.Fd()), temp, int(dir.Fd()), leaf); err != nil {
		return fmt.Errorf("publish skill file %s: %w", name, err)
	}
	return m.barrier(dir, name)
}
func checkExisting(dir *os.File, name string) error {
	file, err := openFile(dir, name, unix.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close checked skill file: %w", err)
	}
	return nil
}
func writeFile(file *os.File, data []byte) error {
	n, err := file.Write(data)
	if err != nil {
		return fmt.Errorf("write skill file: %w", err)
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync skill file: %w", err)
	}
	return nil
}
func (m *Manager) barrier(dir *os.File, label string) error {
	if m.syncDir != nil {
		if err := m.syncDir(dir, label); err != nil {
			return fmt.Errorf("sync skill directory for %s: %w", label, err)
		}
		return nil
	}
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync skill directory for %s: %w", label, err)
	}
	return nil
}
func (m *Manager) removeAt(root *os.File, name string) error {
	dir, leaf, err := metadataParent(root, name, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if err := checkExisting(dir, leaf); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(dir.Fd()), leaf, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove managed skill file: %w", err)
	}
	return m.barrier(dir, name)
}
func lockDirectory(ctx context.Context, dir *os.File) (*os.File, error) {
	file, err := openFile(dir, ".skills.lock", unix.O_RDWR|unix.O_CREAT, 0600)
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
			return fmt.Errorf("wait for skill lock: %w", err)
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("lock skill registry: %w", err)
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("wait for skill lock: %w", wait.Err())
		case <-ticker.C:
		}
	}
}

func readManaged(root *os.File, name string) ([]byte, bool, error) {
	data, err := readAt(root, name, maximumBundle)
	if errors.Is(err, errContentTooLarge) {
		return nil, true, nil
	}
	return data, false, err
}
