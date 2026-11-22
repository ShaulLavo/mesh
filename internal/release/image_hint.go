package release

import (
	"os"
	"strings"
	"sync"
)

// A process that starts a copy of its own executable already knows that
// image's digest. Hashing 60 MB again in every child cost each worker start
// 100 to 300 ms, so the parent hands the digest over together with a stamp of
// the file it belongs to. The child trusts it only while its own pinned image
// carries the same stamp, so a replaced or different binary is hashed as before.

type imageDigest struct{ stamp, digest string }

var (
	imageMu sync.Mutex
	// known is the digest of this process's image once it is available,
	// whether hashed here or adopted from the parent.
	known imageDigest
	// adopted is a parent's hint, consulted when the image is first read.
	adopted imageDigest
)

const hintSeparator = ";"

// DigestHintFor returns the hint for a child started from path. It is empty
// unless this process already knows its own digest and path is that same file;
// it never hashes, so a caller that has not asked for Current pays nothing.
func DigestHintFor(path string) string {
	imageMu.Lock()
	image := known
	imageMu.Unlock()
	if image.stamp == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || imageStamp(info) != image.stamp {
		return ""
	}
	return image.stamp + hintSeparator + image.digest
}

// AdoptDigestHint records a parent's hint. Call it before Current; once the
// image has been read it has no effect.
func AdoptDigestHint(hint string) {
	stamp, digest, ok := strings.Cut(hint, hintSeparator)
	if !ok || stamp == "" || digest == "" {
		return
	}
	imageMu.Lock()
	adopted = imageDigest{stamp: stamp, digest: digest}
	imageMu.Unlock()
}

// adoptedDigest returns the parent's digest when it describes this exact image.
func adoptedDigest(stamp string) (string, bool) {
	imageMu.Lock()
	defer imageMu.Unlock()
	return adopted.digest, stamp != "" && adopted.stamp == stamp
}

func rememberImage(stamp, digest string) {
	if stamp == "" {
		return
	}
	imageMu.Lock()
	known = imageDigest{stamp: stamp, digest: digest}
	imageMu.Unlock()
}
