package wake

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
)

func wakeDir(stateDir string) (string, error) {
	if stateDir == "" {
		return "", errors.New("wake state directory is empty")
	}
	dir := filepath.Join(stateDir, "wake")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create wake state directory: %w", err)
	}
	return dir, nil
}

func lockFile(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // callers derive lock paths from fixed filenames or validated Ed25519 target IDs
	if err != nil {
		return nil, fmt.Errorf("open wake lock: %w", err)
	}
	for {
		if err = ctx.Err(); err != nil {
			break
		}
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			break
		}
		if err = wait(ctx, 10*time.Millisecond); err != nil {
			break
		}
	}
	_ = file.Close()
	return nil, fmt.Errorf("lock wake state: %w", err)
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func readJSON(path string, value any) error {
	file, err := os.Open(path) //nolint:gosec // callers derive state paths from fixed filenames or validated Ed25519 target IDs
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	contents, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil {
		return fmt.Errorf("read wake state %s: %w", path, err)
	}
	if len(contents) > 64<<10 {
		return fmt.Errorf("wake state %s exceeds 64 KiB", path)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode wake state %s: %w", path, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("wake state %s has trailing data", path)
	}
	return nil
}

func writeJSON(path string, value any) error {
	contents, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode wake state: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".wake-*")
	if err != nil {
		return fmt.Errorf("create wake state: %w", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	defer func() { _ = file.Close() }()
	if _, err := file.Write(append(contents, '\n')); err != nil {
		return fmt.Errorf("write wake state: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync wake state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close wake state: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("publish wake state: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open wake state directory: %w", err)
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
