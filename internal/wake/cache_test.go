package wake

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCachePutContextCancelsBlockedLock(t *testing.T) {
	cache, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	grant := testGrant(t)
	lock, err := lockFile(context.Background(), cache.path(grant.TargetID)+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := cache.PutContext(ctx, grant); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked put: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cache write ignored cancellation")
	}
	if _, err := cache.Get(grant.TargetID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled put wrote a grant: %v", err)
	}
}

func TestCanceledSenderDoesNotWaitForCacheLock(t *testing.T) {
	stateDir := t.TempDir()
	sender, err := NewSenderWithOptions(stateDir, SenderOptions{Discover: func(context.Context) (NIC, error) { t.Fatal("discovery after canceled cache write"); return NIC{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	grant := testGrant(t)
	lock, err := lockFile(context.Background(), filepath.Join(sender.cache.dir, "cache.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := sender.Probe(ctx, grant); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled probe: %v", err)
	}
}

func TestCacheCapacityIncludesOrphanedLocksAndAllowsExistingTargets(t *testing.T) {
	cache, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var first Grant
	for range MaximumCachedTargets {
		grant := freshIdentityGrant(t)
		if err := os.WriteFile(cache.path(grant.TargetID)+".lock", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		first = grant
	}
	rejected := freshIdentityGrant(t)
	if err := cache.Put(rejected); !errors.Is(err, ErrCacheFull) {
		t.Fatalf("full cache accepted unknown target: %v", err)
	}
	if _, err := os.Stat(cache.path(rejected.TargetID) + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected target left a lock file: %v", err)
	}
	if err := cache.Put(first); err != nil {
		t.Fatalf("known target failed in full cache: %v", err)
	}
	if _, err := cache.Get(first.TargetID); err != nil {
		t.Fatal(err)
	}
}

func freshIdentityGrant(t *testing.T) Grant {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nic := testNIC()
	now := time.Now().UTC()
	grant := Grant{TargetID: base64.RawURLEncoding.EncodeToString(public), Enabled: true, Revision: 1, IssuedAt: now, ExpiresAt: now.Add(GrantLifetime), NIC: &nic}
	grant.Signature = ed25519.Sign(private, grantTranscript(grant))
	return grant
}

func TestCooldownReusesOneFileAfterExpiry(t *testing.T) {
	now := time.Now()
	sender, err := NewSenderWithOptions(t.TempDir(), SenderOptions{Discover: func(context.Context) (NIC, error) { return testNIC(), nil }, Transmit: func(context.Context, NIC, string) error { return nil }, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	grant := testGrant(t)
	for range 3 {
		sent, err := sender.Send(context.Background(), grant)
		if err != nil || !sent {
			t.Fatalf("send = %v, %v", sent, err)
		}
		now = now.Add(Cooldown)
	}
	entries, err := os.ReadDir(sender.cache.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("repeated sends left %d files, want grant, target lock, cooldown and admission lock", len(entries))
	}
}
