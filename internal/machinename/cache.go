package machinename

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/identity"
)

const cacheDirectory = "machine-names"

func CachedClaim(directory, owner string) (Claim, error) {
	return cachedClaimWithSync(directory, owner, syncCacheRoot)
}

func cachedClaimWithSync(directory, owner string, syncRoot func(*os.Root) error) (Claim, error) {
	if directory == "" {
		return Claim{}, errors.New("machine name cache directory is empty")
	}
	if _, err := identity.IdentityKey(owner); err != nil {
		return Claim{}, fmt.Errorf("cached machine name identity: %w", err)
	}
	root, err := openCacheDirectory(directory, false, syncRoot)
	if errors.Is(err, os.ErrNotExist) {
		return Claim{}, nil
	}
	if err != nil {
		return Claim{}, err
	}
	defer root.Close() //nolint:errcheck // anchored cache descriptor cleanup
	return readCachedClaim(root, owner, syncRoot)
}

func readCachedClaim(root *os.Root, owner string, syncRoot func(*os.Root) error) (Claim, error) {
	file, err := openPrivateCacheFile(root, owner+".json", os.O_RDONLY, false)
	if errors.Is(err, os.ErrNotExist) {
		return Claim{}, nil
	}
	if err != nil {
		return Claim{}, fmt.Errorf("open cached machine name: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only descriptor cleanup
	current, err := readRecordFile(file)
	if err != nil {
		return Claim{}, err
	}
	if current.Version != 1 || current.PreviousRevision != nil {
		return Claim{}, errors.New("cached machine name has an invalid version or receipt")
	}
	if err := ValidateClaim(owner, current.Claim); err != nil {
		return Claim{}, err
	}
	// An atomic replacement can be visible after its publisher's directory sync failed.
	if err := syncRoot(root); err != nil {
		return Claim{}, err
	}
	return current.Claim, nil
}

// RememberClaim receives only claims from the authenticated owner connection.
// Per-ID files keep observations independent of address and dashboard writes.
func RememberClaim(ctx context.Context, directory, owner string, next Claim) (bool, error) {
	return rememberClaimWithSync(ctx, directory, owner, next, syncCacheRoot)
}

func rememberClaimWithSync(ctx context.Context, directory, owner string, next Claim, syncRoot func(*os.Root) error) (bool, error) {
	if directory == "" {
		return false, errors.New("machine name cache directory is empty")
	}
	if err := ValidateClaim(owner, next); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("cache machine name: %w", err)
	}
	root, err := openCacheDirectory(directory, true, syncRoot)
	if err != nil {
		return false, err
	}
	defer root.Close() //nolint:errcheck // anchored cache descriptor cleanup
	lock, err := openPrivateCacheFile(root, owner+".lock", os.O_RDWR, true)
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
	current, err := readCachedClaim(root, owner, syncRoot)
	if err != nil {
		return false, err
	}
	changed, err := acceptClaim(current, next)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("publish cached machine name: %w", err)
	}
	_, err = publishRecordRoot(root, owner+".json", record{Version: 1, Claim: next}, func() error { return syncRoot(root) })
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

func openCacheDirectory(directory string, create bool, syncRoot func(*os.Root) error) (*os.Root, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve machine name cache directory: %w", err)
	}
	root, ancestor, err := openCacheAncestor(absolute)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(ancestor, filepath.Join(absolute, cacheDirectory))
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("locate cache directory: %w", err)
	}
	for _, name := range strings.Split(relative, string(filepath.Separator)) {
		next, err := openPrivateCacheChild(root, name, create, syncRoot)
		_ = root.Close() //nolint:errcheck // next owns its independent directory descriptor
		if err != nil {
			return nil, err
		}
		root = next
	}
	return root, nil
}

func openPrivateCacheChild(parent *os.Root, name string, create bool, syncRoot func(*os.Root) error) (*os.Root, error) {
	if create {
		if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create private cache directory: %w", err)
		}
	}
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("inspect private cache directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("machine name cache requires private real directories with permissions 0700")
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("open private cache directory: %w", err)
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		_ = root.Close()
		return nil, errors.New("machine name cache directory changed while opening")
	}
	// Re-sync existing entries too: a prior creation may have failed this barrier.
	if err := syncRoot(parent); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

func syncCacheRoot(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open machine name cache directory: %w", err)
	}
	defer file.Close() //nolint:errcheck // directory descriptor cleanup
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync machine name cache directory: %w", err)
	}
	return nil
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

func openPrivateCacheFile(root *os.Root, filename string, flag int, create bool) (*os.File, error) {
	if create {
		// Exclusive creation avoids Darwin's contended O_CREATE open path and never follows links.
		file, err := root.OpenFile(filename, flag|os.O_CREATE|os.O_EXCL|syscall.O_NONBLOCK, 0o600)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create private cache file: %w", err)
		}
	}
	info, err := root.Lstat(filename)
	if err != nil {
		return nil, fmt.Errorf("inspect private cache file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("machine name cache requires regular files with permissions 0600")
	}
	// Root.OpenFile resolves contained links even when its caller passes O_NOFOLLOW.
	file, err := root.OpenFile(filename, flag|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open private cache file: %w", err)
	}
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		_ = file.Close()
		return nil, errors.New("machine name cache file changed while opening")
	}
	return file, nil
}
