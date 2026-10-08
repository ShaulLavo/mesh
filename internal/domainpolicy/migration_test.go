package domainpolicy

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDeploymentMigrationPreservesExistingState(t *testing.T) {
	for _, source := range []string{"private-name", "requested", "catalog"} {
		t.Run(source, func(t *testing.T) {
			active = Policy{}
			initialize = sync.Once{}
			t.Cleanup(func() { active = Policy{}; initialize = sync.Once{} })
			state := t.TempDir()
			path := filepath.Join(t.TempDir(), "config", "domains.json")
			expected := legacyDeploymentDomain
			switch source {
			case "private-name":
				expected = "old.example"
				slot := filepath.Join(state, "private-tls", "live")
				if err := os.MkdirAll(slot, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(slot, "private-name"), []byte("pc.mesh.old.example\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "catalog":
				if err := os.WriteFile(filepath.Join(state, "mesh.db"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := InitializeDeployment(path, state, source == "requested"); err != nil {
				t.Fatal(err)
			}
			if Primary() != expected || Current().LegacyCertificateDomain != expected {
				t.Fatalf("migration policy: %+v", Current())
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("policy permissions: %v %v", info, err)
			}
		})
	}
}

func TestNativeFirstRunNeedsNoPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "domains.json")
	if err := InitializeDeployment(path, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("native entry created policy state: %v", err)
	}
}

func TestMigrationPreservesExistingPolicy(t *testing.T) {
	t.Cleanup(func() { active = Policy{}; initialize = sync.Once{} })
	path := filepath.Join(t.TempDir(), "domains.json")
	contents := []byte(`{"primary":"new.example","aliases":["old.example"],"legacyCertificateDomain":"old.example"}`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	seedPolicySlots(t, filepath.Join(state, "private-tls"), "slots")
	if err := InitializeDeployment(path, state, true); err != nil {
		t.Fatal(err)
	}
	if Primary() != "new.example" {
		t.Fatal("migration replaced an explicit policy")
	}
	if err := publishMigration(path, Policy{Primary: legacyDeploymentDomain}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // path belongs to the isolated policy fixture
	if err != nil || string(got) != string(contents) {
		t.Fatalf("migration overwrote configuration: %s %v", got, err)
	}
}

func TestMigrationRejectsMalformedLegacyState(t *testing.T) {
	slot := filepath.Join(t.TempDir(), "private-tls", "live")
	if err := os.MkdirAll(slot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slot, "private-name"), []byte("foreign.invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "domains.json")
	if err := InitializeDeployment(path, filepath.Dir(filepath.Dir(slot)), false); err == nil {
		t.Fatal("malformed legacy name was migrated")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("rejected migration wrote policy: %v", err)
	}
}

func TestMigrationRejectsMissingPolicyWithDomainSlots(t *testing.T) {
	for _, fixture := range []struct{ profile, entry string }{
		{"private-tls", "slots"}, {"certificates/public-edge", "slots"},
		{"private-tls", "empty"}, {"certificates/public-edge", "empty"},
		{"private-tls", "symlink"}, {"certificates/public-edge", "file"},
	} {
		t.Run(fixture.profile+"/"+fixture.entry, func(t *testing.T) {
			active = Policy{}
			initialize = sync.Once{}
			t.Cleanup(func() { active = Policy{}; initialize = sync.Once{} })
			state := t.TempDir()
			root := filepath.Join(state, fixture.profile)
			seedPolicySlots(t, root, fixture.entry)
			path := filepath.Join(t.TempDir(), "domains.json")
			err := InitializeDeployment(path, state, false)
			var missing *MissingPolicyError
			if !errors.As(err, &missing) || missing.Profile != root || !strings.Contains(err.Error(), "restore domains.json") {
				t.Fatalf("missing policy recovery: %v", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("rejected migration published policy: %v", err)
			}
		})
	}
}

func seedPolicySlots(t *testing.T, root, entry string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "domains")
	switch entry {
	case "slots":
		slot := filepath.Join(path, "new.test", "live")
		if err := os.MkdirAll(slot, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(slot, "private-name"), []byte("host.mesh.new.test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "empty":
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	case "symlink":
		if err := os.Symlink(filepath.Join(root, "missing"), path); err != nil {
			t.Fatal(err)
		}
	case "file":
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrationInfersCertificateDomainWithoutPrivateName(t *testing.T) {
	for _, profile := range []string{"private-tls", "certificates/public-edge"} {
		t.Run(profile, func(t *testing.T) {
			active = Policy{}
			initialize = sync.Once{}
			t.Cleanup(func() { active = Policy{}; initialize = sync.Once{} })
			state := t.TempDir()
			expected := "other.test"
			wildcard := "*." + expected
			if profile == "private-tls" {
				wildcard = "*.mesh." + expected
			}
			seedLegacyCertificate(t, filepath.Join(state, profile, "live"), wildcard)
			path := filepath.Join(t.TempDir(), "domains.json")
			if err := InitializeDeployment(path, state, false); err != nil {
				t.Fatal(err)
			}
			if Primary() != expected || Current().LegacyCertificateDomain != expected {
				t.Fatalf("certificate-only policy: %+v", Current())
			}
		})
	}
}

func seedLegacyCertificate(t *testing.T, slot, wildcard string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// Expired certificates still identify legacy storage during recovery.
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{wildcard}, NotBefore: time.Unix(1, 0), NotAfter: time.Unix(2, 0)}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, public, private)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(der)
	fingerprint := hex.EncodeToString(digest[:])
	if err := os.MkdirAll(filepath.Join(slot, fingerprint), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slot, fingerprint, "fullchain.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slot, "current"), []byte(fingerprint+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlyDeploymentInfersWithoutPublishing(t *testing.T) {
	for _, source := range []string{"private-name", "catalog", "fresh", "policy-era", "configured"} {
		t.Run(source, func(t *testing.T) {
			active = Policy{}
			initialize = sync.Once{}
			t.Cleanup(func() { active = Policy{}; initialize = sync.Once{} })
			state := t.TempDir()
			config := t.TempDir()
			path := filepath.Join(config, "domains.json")
			expected := ""
			switch source {
			case "private-name":
				expected = "other.test"
				slot := filepath.Join(state, "private-tls", "live")
				if err := os.MkdirAll(slot, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(slot, "private-name"), []byte("host.mesh."+expected+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "catalog":
				expected = legacyDeploymentDomain
				if err := os.WriteFile(filepath.Join(config, "hosts.json"), []byte(`{"version":1,"hosts":[]}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "configured":
				expected = "other.test"
				if err := os.WriteFile(path, []byte(`{"primary":"other.test"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "policy-era":
				seedPolicySlots(t, filepath.Join(state, "private-tls"), "slots")
			}
			if err := os.Chmod(config, 0o500); err != nil { //nolint:gosec // test-owned directory needs owner read and traversal while refusing writes
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chmod(config, 0o700); err != nil { //nolint:gosec // restore owner access to the isolated directory for test cleanup
					t.Error(err)
				}
			})
			err := InitializeReadOnlyDeployment(path, state)
			if source == "policy-era" {
				var missing *MissingPolicyError
				if !errors.As(err, &missing) {
					t.Fatalf("read-only recovery error: %v", err)
				}
			} else if err != nil || Primary() != expected {
				t.Fatalf("read-only policy: %+v %v", Current(), err)
			}
			if _, err := os.Stat(path); source != "configured" && !os.IsNotExist(err) {
				t.Fatalf("read-only entry published policy: %v", err)
			}
			entries, err := os.ReadDir(config)
			if err != nil {
				t.Fatal(err)
			}
			expectedEntries := 0
			if source == "catalog" || source == "configured" {
				expectedEntries = 1
			}
			if len(entries) != expectedEntries {
				t.Fatalf("read-only configuration changed: %v", entries)
			}
		})
	}
}
