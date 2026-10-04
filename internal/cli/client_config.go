package cli

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
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sys/unix"
)

const clientConfigLimit = 2 << 20

// Retirement is an activation write; the strict reader never accepts retired fields.
func retireClientAliases(ctx context.Context) error {
	return retireClientAliasesWithSettlement(ctx, syncClientConfig)
}

func retireClientAliasesWithSettlement(ctx context.Context, settle func(*os.File) error) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	dir, err := openClientConfigDirectory(filepath.Dir(path), false)
	if err != nil || dir == nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	next, changed, err := readClientRetirement(dir, path)
	if err != nil || next == nil {
		return err
	}
	if !changed {
		// A visible rename may need directory settlement after an earlier failure.
		// Unchanged reads need no writer lock or directory write permission.
		return settle(dir)
	}
	ready, err := clientConfigCanRetire(ctx)
	if err != nil || !ready {
		return err
	}
	return withClientConfigLock(ctx, false, func(locked *os.File, path string) error {
		next, changed, err := readClientRetirement(locked, path)
		if err != nil || next == nil {
			return err
		}
		if !changed {
			return settle(locked)
		}
		return publishClientConfig(locked, next, settle)
	})
}

func readClientRetirement(dir *os.File, path string) ([]byte, bool, error) {
	contents, err := readClientConfig(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read host config %s: %w", path, err)
	}
	next, changed, err := withoutClientAliases(contents)
	if err != nil {
		return nil, false, fmt.Errorf("retire obsolete host config fields %s: %w; correct this file and retry", path, err)
	}
	if _, err := parseHostConfig(next, path); err != nil {
		return nil, false, err
	}
	return next, changed, nil
}

func withoutClientAliases(contents []byte) ([]byte, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	if err := checkConfigJSON(decoder, 0); err != nil {
		return nil, false, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, false, errors.New("trailing configuration data")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(contents, &object); err != nil {
		return nil, false, fmt.Errorf("decode host config object: %w", err)
	}
	if _, exists := object["hosts"]; !exists {
		return contents, false, nil
	}
	var hosts []map[string]json.RawMessage
	if err := json.Unmarshal(object["hosts"], &hosts); err != nil {
		return nil, false, fmt.Errorf("host array: %w", err)
	}
	changed, err := removeHostAliases(hosts)
	if err != nil {
		return nil, false, err
	}
	if !changed {
		return contents, false, nil
	}
	encoded, err := json.Marshal(hosts)
	if err != nil {
		return nil, false, fmt.Errorf("encode retained host records: %w", err)
	}
	object["hosts"] = encoded
	next, err := json.MarshalIndent(object, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("encode retired host config: %w", err)
	}
	return append(next, '\n'), true, nil
}

func removeHostAliases(hosts []map[string]json.RawMessage) (bool, error) {
	changed := false
	for _, host := range hosts {
		value, exists := host["alias"]
		if !exists {
			continue
		}
		value = bytes.TrimSpace(value)
		if len(value) == 0 || value[0] != '"' {
			return false, errors.New("obsolete host alias must be a JSON string")
		}
		var obsolete string
		if err := json.Unmarshal(value, &obsolete); err != nil {
			return false, fmt.Errorf("decode obsolete host alias string: %w", err)
		}
		delete(host, "alias")
		changed = true
	}
	return changed, nil
}

func checkConfigJSON(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("configuration nesting exceeds 64 levels")
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("read configuration token: %w", err)
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	keys := map[string]bool{}
	for decoder.More() {
		if delimiter == '{' {
			if err := checkConfigKey(decoder, keys); err != nil {
				return err
			}
		}
		if err := checkConfigJSON(decoder, depth+1); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("close configuration object: %w", err)
	}
	return nil
}

func checkConfigKey(decoder *json.Decoder, keys map[string]bool) error {
	key, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("read configuration key: %w", err)
	}
	name, ok := key.(string)
	if !ok {
		return errors.New("configuration object key must be a string")
	}
	folded := foldConfigKey(name)
	if keys[folded] {
		return errors.New("configuration contains duplicate or case-ambiguous object keys")
	}
	keys[folded] = true
	return nil
}

func foldConfigKey(name string) string {
	// encoding/json matches struct fields by Unicode simple folding as well as case.
	return strings.Map(func(r rune) rune {
		folded := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < folded {
				folded = next
			}
		}
		return folded
	}, name)
}

func withClientConfigLock(ctx context.Context, create bool, operation func(*os.File, string) error) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	dir, err := openClientConfigDirectory(filepath.Dir(path), create)
	if err != nil || dir == nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	lock, err := openClientConfigFile(dir, ".hosts.lock", unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		return fmt.Errorf("open host config writer lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := acquireClientConfigLock(ctx, lock); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("host config operation cancelled: %w", err)
	}
	return operation(dir, path)
}

func openClientConfigDirectory(directory string, create bool) (*os.File, error) {
	if create {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create Mesh config directory: %w", err)
		}
	}
	fd, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if !create && errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open Mesh config directory %s: %w", directory, err)
	}
	dir := os.NewFile(uintptr(fd), directory)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("inspect Mesh config directory: %w", err)
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o022 != 0 { //nolint:gosec // the OS defines effective UIDs as unsigned 32-bit values
		_ = dir.Close()
		return nil, errors.New("Mesh config directory must be owned by this user and writable only by this user")
	}
	return dir, nil
}

func acquireClientConfigLock(ctx context.Context, lock *os.File) error {
	wait, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	for {
		if err := wait.Err(); err != nil {
			return fmt.Errorf("acquire host config writer lock: %w", err)
		}
		err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("lock host config: %w", err)
		}
		if err := waitState(wait, 20*time.Millisecond); err != nil {
			return err
		}
	}
}

func openClientConfigFile(dir *os.File, name string, flags int, mode uint32) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, mode)
	if err != nil {
		return nil, fmt.Errorf("open host config file: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect host config file: %w", err)
	}
	privateWriter := name != hostConfigName && stat.Mode&0o077 != 0
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || privateWriter { //nolint:gosec // the OS defines effective UIDs as unsigned 32-bit values

		_ = file.Close()
		return nil, errors.New("host config files must be regular files owned by this user with one link; writer files must be private")
	}
	// Legacy readable books need no write access; tighten writable books through
	// the validated descriptor without changing their contents or inode.
	if stat.Mode&0o022 != 0 {
		if err := file.Chmod(os.FileMode(stat.Mode & 0o700)); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("restrict host config write permissions: %w", err)
		}
	}
	return file, nil
}

func readClientConfig(dir *os.File) ([]byte, error) {
	file, err := openClientConfigFile(dir, hostConfigName, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	contents, err := io.ReadAll(io.LimitReader(file, clientConfigLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read bounded host config file: %w", err)
	}
	if len(contents) > clientConfigLimit {
		return nil, errors.New("host config exceeds 2 MiB")
	}
	return contents, nil
}

func publishClientConfig(dir *os.File, contents []byte, settle func(*os.File) error) error {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("generate host config temporary filename: %w", err)
	}
	name := ".hosts-" + hex.EncodeToString(random[:]) + ".json"
	file, err := openClientConfigFile(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary host config: %w", err)
	}
	defer func() { _ = unix.Unlinkat(int(dir.Fd()), name, 0) }()
	defer func() { _ = file.Close() }()
	if _, err := file.Write(contents); err != nil {
		return fmt.Errorf("write temporary host config: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync temporary host config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary host config: %w", err)
	}
	if err := unix.Renameat(int(dir.Fd()), name, int(dir.Fd()), hostConfigName); err != nil {
		return fmt.Errorf("publish host config: %w", err)
	}
	return settle(dir)
}

func syncClientConfig(dir *os.File) error {
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("settle host config directory: %w", err)
	}
	return nil
}
