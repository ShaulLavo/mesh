package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLoadOrCreateIsStableAndSecure(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")

	host1, private1, err := LoadOrCreate(stateDir)
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}

	wantID := base64.RawURLEncoding.EncodeToString(host1.PublicKey)
	if host1.ID != wantID {
		t.Fatalf("host ID = %q, want encoded public key %q", host1.ID, wantID)
	}
	if !bytes.Equal(private1.Public().(ed25519.PublicKey), host1.PublicKey) {
		t.Fatal("returned public and private keys do not belong to the same keypair")
	}

	keyPath := filepath.Join(stateDir, "identity.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat identity key: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("identity key permissions = %04o, want 0600", got)
	}

	host2, private2, err := LoadOrCreate(stateDir)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if host2.ID != host1.ID || !bytes.Equal(host2.PublicKey, host1.PublicKey) {
		t.Fatalf("host changed between loads: first %#v, second %#v", host1, host2)
	}
	if !bytes.Equal(private2, private1) {
		t.Fatal("private key changed between loads")
	}
}

func TestLoadOrCreatePublishesOneIdentityConcurrently(t *testing.T) {
	const callers = 24

	type result struct {
		id      string
		private []byte
		err     error
	}

	stateDir := filepath.Join(t.TempDir(), "state")
	start := make(chan struct{})
	results := make(chan result, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			host, private, err := LoadOrCreate(stateDir)
			results <- result{id: host.ID, private: private, err: err}
		}()
	}

	close(start)
	wg.Wait()
	close(results)

	var first result
	for got := range results {
		if got.err != nil {
			t.Fatalf("LoadOrCreate: %v", got.err)
		}
		if first.id == "" {
			first = got
			continue
		}
		if got.id != first.id || !bytes.Equal(got.private, first.private) {
			t.Fatalf("concurrent caller got a different identity: %q and %q", first.id, got.id)
		}
	}
}

func TestLoadOrCreateRejectsInvalidState(t *testing.T) {
	if _, _, err := LoadOrCreate(""); err == nil || !strings.Contains(err.Error(), "state directory") {
		t.Fatalf("empty state directory error = %v, want a named state directory error", err)
	}

	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "identity.key"), []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write corrupt key: %v", err)
	}
	if _, _, err := LoadOrCreate(stateDir); err == nil || !strings.Contains(err.Error(), "identity key") {
		t.Fatalf("corrupt identity error = %v, want a named identity key error", err)
	}
}
