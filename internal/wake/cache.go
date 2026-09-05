package wake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const MaximumCachedTargets = 256

var ErrCacheFull = errors.New("wake cache has reached its target limit")

type Cache struct{ dir string }

func NewCache(stateDir string) (*Cache, error) {
	dir, err := wakeDir(stateDir)
	if err != nil {
		return nil, err
	}
	return &Cache{dir: dir}, nil
}

// Get retains expired policies so their revisions still reject revoked grants.
func (c *Cache) Get(targetID string) (Grant, error) {
	if _, err := targetKey(targetID); err != nil {
		return Grant{}, err
	}
	var grant Grant
	if err := readJSON(c.path(targetID), &grant); err != nil {
		return Grant{}, err
	}
	if grant.TargetID != targetID {
		return Grant{}, errors.New("wake cache target identity mismatch")
	}
	if err := validateSignedGrant(grant); err != nil {
		return Grant{}, fmt.Errorf("read cached wake permission: %w", err)
	}
	return grant, nil
}

func (c *Cache) Put(grant Grant) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.PutContext(ctx, grant)
}

func (c *Cache) PutContext(ctx context.Context, grant Grant) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateGrant(grant, time.Now()); err != nil {
		return err
	}
	admission, err := lockFile(ctx, filepath.Join(c.dir, "cache.lock"))
	if err != nil {
		return err
	}
	defer func() { _ = admission.Close() }()
	if err := c.admit(grant.TargetID); err != nil {
		return err
	}
	lock, err := lockFile(ctx, c.path(grant.TargetID)+".lock")
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	prior, err := c.Get(grant.TargetID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := validateCurrentPolicy(prior, grant); err != nil {
			return err
		}
		if grant.Revision == prior.Revision && (grant.IssuedAt.Before(prior.IssuedAt) || grant.ExpiresAt.Before(prior.ExpiresAt)) {
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeJSON(c.path(grant.TargetID), grant)
}

func (c *Cache) admit(targetID string) error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return fmt.Errorf("read wake cache directory: %w", err)
	}
	identities := make(map[string]struct{})
	for _, entry := range entries {
		id := cachedIdentity(entry.Name())
		if id == "" {
			continue
		}
		if id == targetID {
			return nil
		}
		identities[id] = struct{}{}
	}
	if len(identities) >= MaximumCachedTargets {
		return ErrCacheFull
	}
	return nil
}

func cachedIdentity(name string) string {
	if !strings.HasPrefix(name, "peer-") {
		return ""
	}
	id, suffix, found := strings.Cut(strings.TrimPrefix(name, "peer-"), ".")
	if !found || (suffix != "json" && suffix != "json.lock" && suffix != "json.cooldown") {
		return ""
	}
	if _, err := targetKey(id); err != nil {
		return ""
	}
	return id
}

func validateCurrentPolicy(prior, next Grant) error {
	if next.Revision < prior.Revision || (next.Revision == prior.Revision && !samePolicy(prior, next)) {
		return ErrStaleGrant
	}
	return nil
}

func (c *Cache) path(targetID string) string { return filepath.Join(c.dir, "peer-"+targetID+".json") }
