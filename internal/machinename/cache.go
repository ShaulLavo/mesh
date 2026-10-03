package machinename

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/identity"
)

const cacheDirectory = "machine-names"

func CachedClaim(directory, owner string) (Claim, error) {
	if directory == "" {
		return Claim{}, errors.New("machine name cache directory is empty")
	}
	if _, err := identity.IdentityKey(owner); err != nil {
		return Claim{}, fmt.Errorf("cached machine name identity: %w", err)
	}
	current, err := readRecord(filepath.Join(directory, cacheDirectory, owner+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return Claim{}, nil
	}
	if err != nil {
		return Claim{}, err
	}
	if current.Version != 1 || current.PreviousRevision != nil {
		return Claim{}, errors.New("cached machine name has an invalid version or receipt")
	}
	if err := ValidateClaim(owner, current.Claim); err != nil {
		return Claim{}, err
	}
	return current.Claim, nil
}

// RememberClaim must receive a claim from the authenticated owner connection.
// Per-ID files keep name observations independent of address and dashboard writes.
func RememberClaim(ctx context.Context, directory, owner string, next Claim) (bool, error) {
	if directory == "" {
		return false, errors.New("machine name cache directory is empty")
	}
	if err := ValidateClaim(owner, next); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("cache machine name: %w", err)
	}
	dir, err := createCacheDirectory(directory)
	if err != nil {
		return false, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, owner+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600) //nolint:gosec // validated identity forms a fixed private lock name
	if err != nil {
		return false, fmt.Errorf("open machine name cache lock: %w", err)
	}
	defer lock.Close() //nolint:errcheck // close releases the cache writer lock
	info, err := lock.Stat()
	if err != nil {
		return false, fmt.Errorf("inspect machine name cache lock: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return false, errors.New("machine name cache lock must be a regular file with permissions 0600")
	}
	if err := lockCache(ctx, lock); err != nil {
		return false, err
	}
	current, err := CachedClaim(directory, owner)
	if err != nil {
		return false, err
	}
	changed, err := acceptClaim(current, next)
	if err != nil {
		return false, err
	}
	if !changed {
		// A prior publisher can fail directory sync after replacing the record.
		return false, syncDirectory(dir)
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("publish cached machine name: %w", err)
	}
	_, err = publishRecord(dir, owner+".json", record{Version: 1, Claim: next}, syncDirectory)
	return err == nil, err
}

func lockCache(ctx context.Context, file *os.File) error {
	lockCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return fmt.Errorf("lock machine name cache: %w", err)
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-lockCtx.Done():
			timer.Stop()
			return fmt.Errorf("wait for machine name cache writer: %w", lockCtx.Err())
		case <-timer.C:
		}
	}
}

func createCacheDirectory(directory string) (string, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve machine name cache directory: %w", err)
	}
	root, ancestor, err := openCacheAncestor(absolute)
	if err != nil {
		return "", err
	}
	defer root.Close() //nolint:errcheck // directory descriptor cleanup
	dir := filepath.Join(absolute, cacheDirectory)
	relative, err := filepath.Rel(ancestor, dir)
	if err != nil {
		return "", fmt.Errorf("locate machine name cache directory: %w", err)
	}
	if err := root.MkdirAll(relative, 0o700); err != nil {
		return "", fmt.Errorf("create machine name cache: %w", err)
	}
	return dir, nil
}

func openCacheAncestor(directory string) (*os.Root, string, error) {
	ancestor := directory
	for {
		root, err := os.OpenRoot(ancestor)
		if errors.Is(err, os.ErrNotExist) && filepath.Dir(ancestor) != ancestor {
			ancestor = filepath.Dir(ancestor)
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("open configuration directory: %w", err)
		}
		return root, ancestor, nil
	}
}
