//go:build !darwin

package updatebootstrap

import (
	"os"
	"testing"
)

func diagDirectBuffer(_ *os.File, size int) ([]byte, func(), error) {
	return make([]byte, size), func() {}, nil
}

func diagControlCache(t *testing.T) {
	t.Logf("DIAG CONTROL uncached-96MiB not run on this platform")
}
