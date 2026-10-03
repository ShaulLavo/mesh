package cli

import (
	"testing"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/transport"
)

func controlFixtureAuthentication(t *testing.T) (*transport.Authentication, string) {
	t.Helper()
	dir, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := identity.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	host, key, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &transport.Authentication{Key: key, Authorize: func(id string) bool { return id == source.ID }}, host.ID
}
