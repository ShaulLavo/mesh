// Package testdomains configures isolated naming fixtures for test processes.
package testdomains

import (
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/domainpolicy"
)

func Setup() func() {
	dir, err := os.MkdirTemp("", "mesh-test-domains-")
	if err != nil {
		panic(err)
	}
	path := filepath.Join(dir, "domains.json")
	if err := os.WriteFile(path, []byte(`{"primary":"mesh.test","aliases":["old.test"],"legacyCertificateDomain":"mesh.test"}`), 0600); err != nil {
		panic(err)
	}
	if err := domainpolicy.Initialize(path); err != nil {
		panic(err)
	}
	return func() { _ = os.RemoveAll(dir) }
}
