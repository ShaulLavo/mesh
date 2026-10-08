package domainpolicy

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
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
	if err := InitializeDeployment(path, t.TempDir(), true); err != nil {
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
