package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/updateinstall"
)

// PruneCache keeps pending fleet artifacts and the latest installation's image.
func PruneCache(ctx context.Context, stateDir, cacheDir string) error {
	store, err := OpenStore(stateDir)
	if err != nil {
		return err
	}
	unlock, err := store.lock()
	if err != nil {
		return err
	}
	defer unlock()
	runs, err := store.List()
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, run := range runs {
		if !run.Done() {
			keep[run.ReleaseDigest] = true
		}
	}
	installation, err := updateinstall.Read(stateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read installation cache references: %w", err)
	}
	if err == nil {
		keep[installation.Request.Manifest.Digest()] = true
	}
	if err := release.PruneCache(ctx, cacheDir, keep); err != nil {
		return fmt.Errorf("prune update cache: %w", err)
	}
	return nil
}

func (c *Coordinator) pruneCache(ctx context.Context) error {
	if c.CacheDir == "" {
		return nil
	}
	stateDir := filepath.Dir(filepath.Dir(c.Store.directory))
	return PruneCache(ctx, stateDir, c.CacheDir)
}
