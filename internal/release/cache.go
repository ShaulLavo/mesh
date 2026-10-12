package release

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"golang.org/x/sys/unix"
)

var cacheDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func lockCache(ctx context.Context, directory string) (func(), error) {
	if directory == "" {
		return nil, errors.New("release: cache directory is empty")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, fmt.Errorf("open release cache lock: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(directory, ".cache.lock"), os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // fixed lock inside the configured cache
	if err != nil {
		return nil, fmt.Errorf("open release cache lock: %w", err)
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) || ctx.Err() != nil {
			_ = file.Close()
			return nil, errors.Join(err, ctx.Err())
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

// PruneCache removes only obsolete manifest directories owned by Download.
// Keep contains pinned installation and unfinished coordinator manifests.
func PruneCache(ctx context.Context, directory string, keep map[string]bool) error {
	unlock, err := lockCache(ctx, directory)
	if err != nil {
		return fmt.Errorf("inspect release cache: %w", err)
	}
	defer unlock()
	root, err := os.OpenRoot(directory)
	if err != nil {
		return fmt.Errorf("inspect release cache: %w", err)
	}
	defer func() { _ = root.Close() }()
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("inspect release cache: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		if keep[entry.Name()] || !cacheDigestPattern.MatchString(entry.Name()) || !entry.IsDir() {
			continue
		}
		if err := root.RemoveAll(entry.Name()); err != nil {
			errs = append(errs, fmt.Errorf("remove obsolete release cache: %w", err))
		}
	}
	return errors.Join(errs...)
}
