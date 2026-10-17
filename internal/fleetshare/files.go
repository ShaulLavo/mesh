package fleetshare

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
	"strings"
	"time"
	"unicode"

	"golang.org/x/sys/unix"
)

const (
	bookName     = "connection-sharing.json"
	maximumBytes = 32 << 20
)

func readState(configDir string) (book, error) {
	dir, err := openDirectory(configDir, false)
	if err != nil {
		return book{}, err
	}
	if dir == nil {
		return book{Version: 1}, nil
	}
	defer func() { _ = dir.Close() }()
	return readBook(dir)
}

func updateState(ctx context.Context, configDir string, operation func(*book) error) error {
	dir, err := openDirectory(configDir, true)
	if err != nil || dir == nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	lock, err := openFile(dir, ".connection-sharing.lock", unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := acquireLock(ctx, lock); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	state, err := readBook(dir)
	if err != nil {
		return err
	}
	if err := operation(&state); err != nil {
		return err
	}
	if err := validateBook(&state); err != nil {
		return err
	}
	return publishBook(dir, state)
}

func openDirectory(path string, create bool) (*os.File, error) {
	if path == "" {
		return nil, errors.New("fleet: configuration directory is empty")
	}
	if create {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("fleet: create configuration directory: %w", err)
		}
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if !create && errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fleet: open configuration directory: %w", err)
	}
	dir := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("fleet: inspect configuration directory: %w", err)
	}
	if int64(stat.Uid) != int64(os.Geteuid()) || stat.Mode&0o022 != 0 {
		_ = dir.Close()
		return nil, errors.New("fleet: configuration directory must be owned by this user and writable only by this user")
	}
	return dir, nil
}

func openFile(dir *os.File, name string, flags int, mode uint32) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, mode)
	if err != nil {
		return nil, fmt.Errorf("fleet: open %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("fleet: inspect %s: %w", name, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || int64(stat.Uid) != int64(os.Geteuid()) || stat.Nlink != 1 || stat.Mode&0o077 != 0 {
		_ = file.Close()
		return nil, errors.New("fleet: configuration files must be private regular files owned by this user with one link")
	}
	return file, nil
}

func acquireLock(ctx context.Context, lock *os.File) error {
	return acquireLockFor(ctx, lock, 1500*time.Millisecond)
}

func acquireLockFor(ctx context.Context, lock *os.File, limit time.Duration) error {
	wait, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := wait.Err(); err != nil {
			return fmt.Errorf("fleet: acquire configuration lock: %w", err)
		}
		err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("fleet: lock configuration: %w", err)
		}
		select {
		case <-wait.Done():
		case <-ticker.C:
		}
	}
}

func readBook(dir *os.File) (book, error) {
	file, err := openFile(dir, bookName, unix.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return book{Version: 1}, nil
	}
	if err != nil {
		return book{}, err
	}
	defer func() { _ = file.Close() }()
	contents, err := io.ReadAll(io.LimitReader(file, maximumBytes+1))
	if err != nil {
		return book{}, fmt.Errorf("fleet: read configuration: %w", err)
	}
	if len(contents) > maximumBytes {
		return book{}, errors.New("fleet: configuration exceeds 32 MiB")
	}
	return decodeBook(contents)
}

func decodeBook(contents []byte) (book, error) {
	if err := checkJSON(json.NewDecoder(bytes.NewReader(contents)), 0); err != nil {
		return book{}, fmt.Errorf("fleet: invalid configuration: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var state book
	if err := decoder.Decode(&state); err != nil {
		return book{}, fmt.Errorf("fleet: decode configuration: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return book{}, errors.New("fleet: configuration has trailing data")
	}
	return state, validateBook(&state)
}

func checkJSON(decoder *json.Decoder, depth int) error {
	if depth > 8 {
		return errors.New("configuration nesting exceeds limits")
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("read JSON token: %w", err)
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	keys := make(map[string]bool)
	for decoder.More() {
		if err := checkJSONEntry(decoder, delimiter, keys, depth); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("close JSON value: %w", err)
	}
	return nil
}

func checkJSONEntry(decoder *json.Decoder, delimiter json.Delim, keys map[string]bool, depth int) error {
	if delimiter == '{' {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("read JSON key: %w", err)
		}
		key, ok := token.(string)
		folded := foldKey(key)
		if !ok || keys[folded] {
			return errors.New("invalid or duplicate JSON key")
		}
		keys[folded] = true
	}
	return checkJSON(decoder, depth+1)
}

func foldKey(key string) string {
	return strings.Map(foldedRune, key)
}

func foldedRune(char rune) rune {
	folded := char
	for next := unicode.SimpleFold(char); next != char; next = unicode.SimpleFold(next) {
		if next < folded {
			folded = next
		}
	}
	return folded
}

func publishBook(dir *os.File, state book) error {
	contents, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("fleet: encode configuration: %w", err)
	}
	if len(contents) >= maximumBytes {
		return errors.New("fleet: configuration exceeds 32 MiB")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("fleet: generate temporary filename: %w", err)
	}
	name := ".connection-sharing-" + hex.EncodeToString(random[:]) + ".json"
	file, err := openFile(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close(); _ = unix.Unlinkat(int(dir.Fd()), name, 0) }()
	if _, err := file.Write(append(contents, '\n')); err != nil {
		return fmt.Errorf("fleet: write configuration: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("fleet: sync configuration: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("fleet: close configuration: %w", err)
	}
	if err := unix.Renameat(int(dir.Fd()), name, int(dir.Fd()), bookName); err != nil {
		return fmt.Errorf("fleet: publish configuration: %w", err)
	}
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("fleet: sync configuration directory: %w", err)
	}
	return nil
}
