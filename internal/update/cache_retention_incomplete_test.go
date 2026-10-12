package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewRetentionRejectsIncompleteInstallationBeforePrune(t *testing.T) {
	state, cache := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "update"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "update", "installation.json"), []byte(`{"schema":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(cache, strings.Repeat("a", 64))
	if err := os.Mkdir(image, 0700); err != nil {
		t.Fatal(err)
	}
	err := PruneCache(t.Context(), state, cache)
	_, statErr := os.Stat(image)
	if err == nil || statErr != nil {
		t.Fatalf("incomplete journal did not fail closed: prune error=%v, retained image stat=%v", err, statErr)
	}
}
