package updateinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const journalLimit = 2 << 20

func transactionDir(stateDir string) string { return filepath.Join(stateDir, "update") }
func journalPath(stateDir string) string {
	return filepath.Join(transactionDir(stateDir), "installation.json")
}

func Read(stateDir string) (Status, error) {
	var status Status
	if err := readJSON(journalPath(stateDir), &status); err != nil {
		return status, err
	}
	if status.Schema != 1 {
		return status, errors.New("unsupported installation journal schema")
	}
	return status, nil
}

func ReadSettings(stateDir string) (Settings, error) {
	status, err := Read(stateDir)
	return status.Settings, err
}

func (e *Engine) Read() (Status, error) { return Read(e.cfg.StateDir) }

func (e *Engine) save(status *Status) error {
	status.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(journalPath(e.cfg.StateDir), append(data, '\n'), 0600)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".update-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err = file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func readJSON(path string, value any) error {
	file, err := os.Open(path) //nolint:gosec // fixed journal, gate, or executable path beneath configured local directories
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, journalLimit+1))
	if err != nil {
		return err
	}
	if len(data) > journalLimit {
		return errors.New("installation journal exceeds size limit")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing installation journal data")
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path) //nolint:gosec // fixed journal, gate, or executable path beneath configured local directories
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func lockInstallation(ctx context.Context, stateDir string) (*os.File, error) {
	dir := transactionDir(stateDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "installation.lock"), os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // fixed installation lock in the configured private state directory
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = file.Close()
			return nil, err
		}
		if err = waitContext(ctx, 25*time.Millisecond); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
}

func unlock(file *os.File) { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // fixed journal, gate, or executable path beneath configured local directories
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyFile(path, expected string) error {
	actual, err := fileDigest(path)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("executable checksum mismatch: %s", path)
	}
	return nil
}

func durableCopy(source, destination, expected string) error {
	in, err := os.Open(source) //nolint:gosec // source is the approved executable or a checksum-verified staged copy
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	file, err := os.CreateTemp(filepath.Dir(destination), ".mesh-copy-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	hash := sha256.New()
	if _, err = io.Copy(io.MultiWriter(file, hash), in); err != nil {
		_ = file.Close()
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		_ = file.Close()
		return errors.New("source changed while staging executable")
	}
	if err = file.Chmod(0755); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), destination); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

func checkMount(path string) error {
	if path == "" {
		return nil
	}
	clean, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("required data mount unavailable: %w", err)
	}
	var mount, parent unix.Stat_t
	if err = unix.Stat(clean, &mount); err != nil {
		return err
	}
	if err = unix.Stat(filepath.Dir(clean), &parent); err != nil {
		return err
	}
	if clean == "/" || mount.Dev == parent.Dev {
		return fmt.Errorf("required data location %s is not a mounted data filesystem", path)
	}
	return nil
}

func checkSpace(path string, minimum uint64) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return err
	}
	if stat.Bsize <= 0 {
		return errors.New("filesystem returned an invalid block size")
	}
	if stat.Bavail < (minimum+uint64(stat.Bsize)-1)/uint64(stat.Bsize) {
		return fmt.Errorf("insufficient free space on %s", path)
	}
	return nil
}
