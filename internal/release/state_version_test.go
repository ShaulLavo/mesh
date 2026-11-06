package release

import (
	"strconv"
	"strings"
	"testing"

	"github.com/shaul/mesh/db/migrations"
)

func TestMetadataStateVersionMatchesLatestMigration(t *testing.T) {
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	latest := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, _ := strings.Cut(entry.Name(), "_")
		version, err := strconv.Atoi(prefix)
		if err != nil {
			t.Fatal(err)
		}
		latest = max(latest, version)
	}
	if got := Metadata().StateVersion; got != latest {
		t.Fatalf("release state version %d, latest migration %d", got, latest)
	}
}
