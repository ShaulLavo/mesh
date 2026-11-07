package access

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const MaximumPolicy = 256 << 10

func scopeDirectory(stateDir string, create bool) (*os.File, error) {
	fd, err := unix.Open(stateDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("access: open state directory: %w", err)
	}
	root := os.NewFile(uintptr(fd), stateDir)
	defer func() { _ = root.Close() }()
	if err := checkDirectory(root, false); err != nil {
		return nil, err
	}
	if create {
		if err := unix.Mkdirat(int(root.Fd()), "access", 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("access: create policy directory: %w", err)
		}
	}
	fd, err = unix.Openat(int(root.Fd()), "access", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("access: open policy directory: %w", err)
	}
	dir := os.NewFile(uintptr(fd), filepath.Join(stateDir, "access"))
	if err := checkDirectory(dir, true); err != nil {
		_ = dir.Close()
		return nil, err
	}
	return dir, nil
}
func checkDirectory(file *os.File, private bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fmt.Errorf("access: inspect directory: %w", err)
	}
	if int64(stat.Uid) != int64(os.Geteuid()) || stat.Mode&0022 != 0 {
		return errors.New("access: directory must be owned and not writable by others")
	}
	if private && stat.Mode&07777 != 0700 {
		return errors.New("access: policy directory must have permissions 0700")
	}
	return nil
}
func privateFile(dir *os.File, name string, flags int) (*os.File, unix.Stat_t, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, unix.Stat_t{}, fmt.Errorf("access: open %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = file.Close()
		return nil, stat, fmt.Errorf("access: inspect file: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || int64(stat.Uid) != int64(os.Geteuid()) || stat.Nlink != 1 || stat.Mode&07777 != 0600 {
		_ = file.Close()
		return nil, stat, errors.New("access: files must be owned 0600 regular files with one link")
	}
	return file, stat, nil
}
func randomName() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("access: random incarnation: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
func strictJSON(data []byte, value any) error {
	if len(data) == 0 || len(data) > MaximumPolicy || !utf8.Valid(data) {
		return errors.New("access: invalid policy encoding or size")
	}
	scanner := json.NewDecoder(bytes.NewReader(data))
	if err := scanValue(scanner, 0); err != nil {
		return err
	}
	if _, err := scanner.Token(); err != io.EOF {
		return errors.New("access: trailing policy data")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("access: decode policy: %w", err)
	}
	return nil
}
func scanValue(decoder *json.Decoder, depth int) error {
	if depth > 8 {
		return errors.New("access: JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("access: decode JSON token: %w", err)
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delimiter == '{' {
		return scanObject(decoder, depth+1)
	}
	if delimiter != '[' {
		return errors.New("access: invalid JSON delimiter")
	}
	for decoder.More() {
		if err := scanValue(decoder, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	if err != nil {
		return fmt.Errorf("access: close JSON array: %w", err)
	}
	return nil
}
func scanObject(decoder *json.Decoder, depth int) error {
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("access: decode JSON key: %w", err)
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return errors.New("access: duplicate or invalid JSON key")
		}
		seen[key] = true
		if err := scanValue(decoder, depth); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("access: close JSON object: %w", err)
	}
	return nil
}

func syncScopeParent(dir *os.File, syncDir func(*os.File) error) error {
	fd, err := unix.Openat(int(dir.Fd()), "..", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("access: open policy parent barrier: %w", err)
	}
	parent := os.NewFile(uintptr(fd), "policy-parent")
	syncErr := syncDir(parent)
	if err := errors.Join(syncErr, parent.Close()); err != nil {
		return fmt.Errorf("access: sync policy parent barrier: %w", err)
	}
	return nil
}
