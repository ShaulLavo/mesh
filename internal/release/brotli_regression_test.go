package release

import (
	"bufio"
	"github.com/andybalholm/brotli"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// Exact byte fixture from upstream fix 5ad9f4053d18, tested without modifying the dependency.
func TestBrotliShortReadRegression(t *testing.T) {
	bad := "\x1b*\x00\x88\x9c\t6N\xa8w7\xbc$00K01\xbc20K012\x98\xc8\x81C[\xa0\xf7\xde$X.\xe91A0Z1$\xe0Y07000\xab#x\xf6\xda%2\x1e"
	for _, kind := range []string{"all-at-once", "short-read", "buffered-short-read"} {
		t.Run(kind, func(t *testing.T) { upstreamCase(t, kind, bad) })
	}
}
func upstreamCase(t *testing.T, kind, input string) {
	var source io.Reader = strings.NewReader(input)
	if kind != "all-at-once" {
		source = iotest.OneByteReader(source)
	}
	if kind == "buffered-short-read" {
		source = bufio.NewReaderSize(source, 64<<10)
	}
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("malformed input panicked: %v", p)
		}
	}()
	n, err := io.Copy(io.Discard, brotli.NewReader(source))
	t.Logf("pattern=%s bytes=%d error=%v", kind, n, err)
	if n != 43 || err == nil || err.Error() != "brotli: excessive input" {
		t.Errorf("malformed input: decoded %d bytes, error %v", n, err)
	}
}
