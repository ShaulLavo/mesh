package release

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func resetImageHints(t *testing.T) {
	t.Helper()
	imageMu.Lock()
	savedKnown, savedAdopted := known, adopted
	known, adopted = imageDigest{}, imageDigest{}
	imageMu.Unlock()
	t.Cleanup(func() {
		imageMu.Lock()
		known, adopted = savedKnown, savedAdopted
		imageMu.Unlock()
	})
}

func imageFile(t *testing.T, name, content string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	return path, hex.EncodeToString(sum[:])
}

func readImage(t *testing.T, path string) Build {
	t.Helper()
	file, err := os.Open(path) //nolint:gosec // a path the test just created
	return readExecutingBuild(file, err)
}

func TestHintNamesOnlyTheImageThatWasHashed(t *testing.T) {
	if imageStamp(mustStat(t, t.TempDir())) == "" {
		t.Skip("no stable file identity on this platform")
	}
	resetImageHints(t)
	path, digest := imageFile(t, "mesh", "first image")
	other, _ := imageFile(t, "other", "first image")
	if hint := DigestHintFor(path); hint != "" {
		t.Fatalf("a hint was offered before any digest was known: %q", hint)
	}
	if got := readImage(t, path).Digest; got != digest {
		t.Fatalf("digest %s, want %s", got, digest)
	}
	hint := DigestHintFor(path)
	if hint == "" {
		t.Fatal("no hint for the image this process hashed")
	}
	if hint[len(hint)-len(digest):] != digest {
		t.Fatalf("hint %q does not end with the image digest", hint)
	}
	if DigestHintFor(other) != "" {
		t.Fatal("a hint was offered for a different file with identical bytes")
	}
}

func TestAdoptedDigestIsUsedOnlyForTheSameImage(t *testing.T) {
	if imageStamp(mustStat(t, t.TempDir())) == "" {
		t.Skip("no stable file identity on this platform")
	}
	resetImageHints(t)
	path, digest := imageFile(t, "mesh", "adopted image")
	stamp := imageStamp(mustStat(t, path))
	AdoptDigestHint(stamp + hintSeparator + "parent-digest")
	if got := readImage(t, path).Digest; got != "parent-digest" {
		t.Fatalf("matching image digest %q was hashed again", got)
	}
	resetImageHints(t)
	replaced, replacedDigest := imageFile(t, "mesh", "a different image")
	AdoptDigestHint(stamp + hintSeparator + "parent-digest")
	if got := readImage(t, replaced).Digest; got != replacedDigest || got == digest {
		t.Fatalf("a hint for another image was trusted: %q", got)
	}
}

func TestMalformedHintsAreIgnored(t *testing.T) {
	resetImageHints(t)
	for _, hint := range []string{"", "no-separator", ";digest", "stamp;"} {
		AdoptDigestHint(hint)
		if _, ok := adoptedDigest("stamp"); ok {
			t.Fatalf("hint %q was adopted", hint)
		}
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
