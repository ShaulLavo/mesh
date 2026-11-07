package cli

import (
	"os"
	"testing"

	"github.com/shaul/mesh/internal/domainpolicy"
	"github.com/shaul/mesh/internal/testdomains"
)

func TestMain(m *testing.M) {
	if path := os.Getenv("MESH_TEST_DOMAIN_POLICY"); path != "" {
		if err := domainpolicy.Initialize(path); err != nil {
			panic(err)
		}
		os.Exit(m.Run())
	}
	cleanup := testdomains.Setup()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
