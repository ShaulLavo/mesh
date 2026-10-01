package release

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMetadataSkipsDigestAndCurrentCachesIt(t *testing.T) {
	original := executingBuild
	t.Cleanup(func() { executingBuild = original })
	reads := 0
	executingBuild = func() Build { reads++; return Build{} }
	metadata := Metadata()
	if metadata.Digest != "" || reads != 0 {
		t.Fatal("metadata hashed the executable")
	}
	executingBuild = newExecutingBuild(executingExecutablePath())
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			build := Current()
			if build.Digest == "" {
				t.Error("Current omitted executable digest")
			}
			build.Digest = ""
			if build != metadata {
				t.Errorf("Current metadata = %+v, want %+v", build, metadata)
			}
		})
	}
	wg.Wait()
}

func TestLazyDigestPinsImageBeforePathReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh")
	image := []byte("loaded executable image")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	current := newExecutingBuild(path)
	replacement := path + ".next"
	if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(image)
	expected := hex.EncodeToString(digest[:])
	for range 2 {
		if got := current().Digest; got != expected {
			t.Fatalf("digest = %q, want loaded image %q", got, expected)
		}
	}
}
