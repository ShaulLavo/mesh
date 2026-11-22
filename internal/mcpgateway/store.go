package mcpgateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const (
	lockFileName  = ".mcp-profiles.lock"
	maxFileBytes  = 16 << 20
	lockWait      = 3 * time.Second
	lockPollEvery = 20 * time.Millisecond
)

// Store reads and writes mcp-profiles.json in one private directory, the
// one that holds hosts.json. Every write takes the profile lock, checks the
// expected revision and replaces the file atomically.
type Store struct {
	dir string
}

// NewStore opens the profiles store in dir without touching the disk.
func NewStore(dir string) *Store { return &Store{dir: dir} }

// Dir is the directory holding the profiles file.
func (s *Store) Dir() string { return s.dir }

// Load reads the current file. A missing file is an empty one at revision 0.
func (s *Store) Load() (File, error) {
	root, err := s.open(false)
	if err != nil || root == nil {
		return NewFile(), err
	}
	defer func() { _ = root.Close() }()
	return readFile(root)
}

// Update applies change under the profile lock. When expected is set, the
// file must still be at that revision. A change that leaves the file equal
// keeps its revision.
func (s *Store) Update(ctx context.Context, expected *uint64, change func(*File) error) (File, error) {
	root, err := s.open(true)
	if err != nil {
		return File{}, err
	}
	defer func() { _ = root.Close() }()
	unlock, err := lock(ctx, root)
	if err != nil {
		return File{}, err
	}
	defer unlock()
	current, err := readFile(root)
	if err != nil {
		return File{}, err
	}
	if expected != nil && *expected != current.Revision {
		return current, fmt.Errorf("%w: expected revision %d, file is at %d", ErrConflict, *expected, current.Revision)
	}
	before, err := json.Marshal(current)
	if err != nil {
		return File{}, fmt.Errorf("encode profiles: %w", err)
	}
	next := cloneFile(current)
	if err := change(&next); err != nil {
		return current, err
	}
	if err := next.Validate(); err != nil {
		return current, err
	}
	next.Revision = current.Revision
	after, err := json.Marshal(next)
	if err != nil {
		return File{}, fmt.Errorf("encode profiles: %w", err)
	}
	if bytes.Equal(before, after) {
		return current, nil
	}
	next.Revision = current.Revision + 1
	if err := writeFile(root, next); err != nil {
		return current, err
	}
	return next, nil
}

func cloneFile(f File) File {
	raw, _ := json.Marshal(f)
	out := NewFile()
	_ = json.Unmarshal(raw, &out)
	return out
}

func (s *Store) open(create bool) (*os.Root, error) {
	if create {
		if err := os.MkdirAll(s.dir, 0o700); err != nil {
			return nil, fmt.Errorf("create Mesh config directory: %w", err)
		}
	}
	info, err := os.Lstat(s.dir)
	if !create && errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect Mesh config directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("mesh config directory %s must be a directory writable only by its owner", s.dir)
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, fmt.Errorf("open Mesh config directory: %w", err)
	}
	return root, nil
}

func lock(ctx context.Context, root *os.Root) (func(), error) {
	file, err := root.OpenFile(lockFileName, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open profile lock: %w", err)
	}
	wait, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = file.Close()
			return nil, fmt.Errorf("lock %s: %w", ProfilesFileName, err)
		}
		select {
		case <-wait.Done():
			_ = file.Close()
			return nil, fmt.Errorf("lock %s: %w", ProfilesFileName, wait.Err())
		case <-time.After(lockPollEvery):
		}
	}
}

func readFile(root *os.Root) (File, error) {
	file, err := root.OpenFile(ProfilesFileName, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return NewFile(), nil
	}
	if err != nil {
		return File{}, fmt.Errorf("open %s: %w", ProfilesFileName, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return File{}, fmt.Errorf("inspect %s: %w", ProfilesFileName, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return File{}, fmt.Errorf("%s must be a private regular file (mode 0600)", ProfilesFileName)
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		return File{}, fmt.Errorf("read %s: %w", ProfilesFileName, err)
	}
	if len(raw) > maxFileBytes {
		return File{}, fmt.Errorf("%w: %s exceeds %d bytes", ErrLimit, ProfilesFileName, maxFileBytes)
	}
	return decodeFile(raw)
}

func writeFile(root *os.Root, f File) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", ProfilesFileName, err)
	}
	raw = append(raw, '\n')
	if len(raw) > maxFileBytes {
		return fmt.Errorf("%w: %s would exceed %d bytes", ErrLimit, ProfilesFileName, maxFileBytes)
	}
	var suffix [8]byte
	_, _ = rand.Read(suffix[:])
	temp := "." + ProfilesFileName + "." + hex.EncodeToString(suffix[:])
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", temp, err)
	}
	_, err = file.Write(raw)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = root.Rename(temp, ProfilesFileName)
	}
	if err != nil {
		_ = root.Remove(temp)
		return fmt.Errorf("write %s: %w", ProfilesFileName, err)
	}
	return syncDir(root)
}

func syncDir(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open config directory for sync: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync config directory: %w", err)
	}
	return nil
}
