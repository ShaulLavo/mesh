package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/hostprofile"
	"golang.org/x/sys/unix"
)

func (s *Store) authorityDirectory() (*os.File, error) {
	directory := filepath.Dir(s.directory)
	fd, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("update: open authority directory: %w", err)
	}
	file := os.NewFile(uintptr(fd), directory)
	if err = privateAuthority(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
func privateAuthority(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("update: inspect authority file: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Geteuid()) || info.Mode().Perm()&0077 != 0 || !info.IsDir() && (!info.Mode().IsRegular() || stat.Nlink != 1) {
		return errors.New("update: authority state must be private, owned and regular")
	}
	return nil
}
func (s *Store) readAuthority(name string, value any) error {
	dir, err := s.authorityDirectory()
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("update: open authority record: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { _ = file.Close() }()
	if err = privateAuthority(file); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumMessage+1))
	if err != nil {
		return fmt.Errorf("update: read authority: %w", err)
	}
	if len(data) > maximumMessage {
		return errors.New("update: authority exceeds size limit")
	}
	if err = decodeAuthority(data, value); err != nil {
		return err
	}
	return s.settlePublication(s.directory)
}
func decodeAuthority(data []byte, value any) error {
	if err := hostprofile.CheckJSON(json.NewDecoder(bytes.NewReader(data)), 0, 32); err != nil {
		return fmt.Errorf("update: check authority JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("update: decode authority: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("update: trailing authority JSON")
	}
	return nil
}
func (s *Store) writeAuthority(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("update: encode authority: %w", err)
	}
	if len(data) > maximumMessage {
		return errors.New("update: authority exceeds size limit")
	}
	dir, err := s.authorityDirectory()
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	id, err := newRandomID()
	if err != nil {
		return err
	}
	temp := ".authority-" + id
	fd, err := unix.Openat(int(dir.Fd()), temp, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return fmt.Errorf("update: create authority: %w", err)
	}
	defer func() { _ = unix.Unlinkat(int(dir.Fd()), temp, 0) }()
	file := os.NewFile(uintptr(fd), temp)
	err = publishAuthority(file, dir, temp, name, data)
	if err != nil {
		return err
	}
	return s.settlePublication(filepath.Dir(s.directory))
}
func publishAuthority(file, dir *os.File, temp, name string, data []byte) error {
	defer func() { _ = file.Close() }()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("update: write authority: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("update: sync authority: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("update: close authority: %w", err)
	}
	if err := unix.Renameat(int(dir.Fd()), temp, int(dir.Fd()), name); err != nil {
		return fmt.Errorf("update: publish authority: %w", err)
	}
	return nil
}
func (s *Store) lockContext(ctx context.Context) (func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	file, err := s.openAuthorityLock()
	if err != nil {
		return nil, err
	}
	if err = waitAuthority(ctx, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = file.Close() }, nil
}
func (s *Store) openAuthorityLock() (*os.File, error) {
	authority, err := s.authorityDirectory()
	if err != nil {
		return nil, err
	}
	defer func() { _ = authority.Close() }()
	fd, err := unix.Openat(int(authority.Fd()), "runs", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("update: open writer directory: %w", err)
	}
	dir := os.NewFile(uintptr(fd), s.directory)
	defer func() { _ = dir.Close() }()
	if err = privateAuthority(dir); err != nil {
		return nil, err
	}
	fd, err = openLockAt(int(dir.Fd()), ".lock")
	if err != nil {
		return nil, fmt.Errorf("update: open authority lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), ".lock")
	if err = privateAuthority(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func waitAuthority(ctx context.Context, file *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("update: acquire authority lock: %w", err)
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("update: lock authority: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("update: acquire authority lock: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func releaseAuthority(file *os.File) func() { return func() { _ = file.Close() } }

func (s *Store) stateDir() string { return filepath.Dir(filepath.Dir(s.directory)) }
