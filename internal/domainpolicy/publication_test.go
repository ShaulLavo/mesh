package domainpolicy

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDomainPublicationPreservesLegacyValueEncoding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "domains.json")
	initial := []byte(`{"primary":"mesh.test","aliases":["old.test"],"legacyCertificateDomain":"m\u0065sh.test"}`)
	if err := os.WriteFile(path, initial, 0600); err != nil {
		t.Fatal(err)
	}
	err := WithDomainFile(t.Context(), path, func(file *DomainFile) error {
		return file.Save(Policy{Primary: "new.test", Aliases: []string{"mesh.test", "old.test"}, LegacyCertificateDomain: file.Policy.LegacyCertificateDomain})
	})
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile("domains.json")
	if err != nil || !bytes.Contains(data, []byte(`"m\u0065sh.test"`)) {
		t.Fatalf("excluded legacy bytes changed: %s %v", data, err)
	}
	if err := WithDomainFile(t.Context(), path, func(file *DomainFile) error {
		return file.Save(Policy{Primary: "new.test", Aliases: []string{"mesh.test"}, LegacyCertificateDomain: "mesh.test"})
	}); err == nil {
		t.Fatal("accepted domain was removed")
	}
}
