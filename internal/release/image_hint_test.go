package release

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
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
	AdoptDigestHint(stamp + hintSeparator + sampleDigest)
	if got := readImage(t, path).Digest; got != sampleDigest {
		t.Fatalf("matching image digest %q was hashed again", got)
	}
	resetImageHints(t)
	replaced, replacedDigest := imageFile(t, "mesh", "a different image")
	AdoptDigestHint(stamp + hintSeparator + sampleDigest)
	if got := readImage(t, replaced).Digest; got != replacedDigest || got == digest {
		t.Fatalf("a hint for another image was trusted: %q", got)
	}
}

// A same-length rewrite that restores the modification time still changes the
// inode's change time, which no process can set.
func TestRewrittenImageWithRestoredMtimeIsNotTrusted(t *testing.T) {
	if imageStamp(mustStat(t, t.TempDir())) == "" {
		t.Skip("no stable file identity on this platform")
	}
	resetImageHints(t)
	path, _ := imageFile(t, "mesh", "original bytes")
	original := mustStat(t, path)
	stamp := imageStamp(original)
	readImage(t, path)
	if err := os.WriteFile(path, []byte("replaced bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	if hint := DigestHintFor(path); hint != "" {
		t.Fatalf("a hint was offered for a rewritten image: %q", hint)
	}
	AdoptDigestHint(stamp + hintSeparator + sampleDigest)
	sum := sha256.Sum256([]byte("replaced bytes"))
	if got := readImage(t, path).Digest; got != hex.EncodeToString(sum[:]) {
		t.Fatalf("a stale digest was adopted for a rewritten image: %q", got)
	}
}

func TestImageReplacedByRenameIsNotTrusted(t *testing.T) {
	if imageStamp(mustStat(t, t.TempDir())) == "" {
		t.Skip("no stable file identity on this platform")
	}
	resetImageHints(t)
	path, _ := imageFile(t, "mesh", "installed image")
	readImage(t, path)
	next := path + ".next"
	if err := os.WriteFile(next, []byte("installed image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	if hint := DigestHintFor(path); hint != "" {
		t.Fatalf("a hint was offered for a replaced image: %q", hint)
	}
}

const sampleDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestHintDigestMustBeASHA256(t *testing.T) {
	resetImageHints(t)
	for _, digest := range []string{"digest", sampleDigest[:63], sampleDigest + "0", strings.ToUpper(sampleDigest), "g" + sampleDigest[1:]} {
		AdoptDigestHint("stamp" + hintSeparator + digest)
		if _, ok := adoptedDigest("stamp"); ok {
			t.Fatalf("digest %q was adopted", digest)
		}
	}
	AdoptDigestHint("stamp" + hintSeparator + sampleDigest)
	if _, ok := adoptedDigest("stamp"); !ok {
		t.Fatal("a well-formed digest was refused")
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
