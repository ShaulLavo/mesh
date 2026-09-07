package updateinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackagePayloadsRequireTheirOwningAdapter(t *testing.T) {
	for _, path := range []string{"/usr/local/bin/mesh", "/usr/local/Caskroom/mesh/1/mesh", "/opt/homebrew/Caskroom/mesh/1/mesh", "/custom/Cellar/mesh/1/mesh", "/nix/store/hash-mesh/bin/mesh", "/snap/mesh/current/bin/mesh"} {
		if err := ValidateInstallationPath(path); err == nil || (!strings.Contains(err.Error(), "package") && !strings.Contains(err.Error(), "Homebrew")) {
			t.Fatalf("unclassified payload %s: %v", path, err)
		}
	}
	path := filepath.Join(t.TempDir(), "mesh")
	if err := os.WriteFile(path, []byte("binary"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateInstallationPath(path); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "mesh")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := ValidateInstallationPath(link); err == nil {
		t.Fatal("unmanaged symlink accepted")
	}
}
