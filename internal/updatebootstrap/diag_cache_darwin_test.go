package updatebootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// diagDirectBuffer turns the file cache off for this open file and returns a
// page-aligned buffer for its reads. Measurement only.
func diagDirectBuffer(file *os.File, size int) ([]byte, func(), error) {
	buffer, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return nil, nil, fmt.Errorf("map direct buffer: %w", err)
	}
	release := func() { _ = unix.Munmap(buffer) }
	if _, err := unix.FcntlInt(file.Fd(), unix.F_NOCACHE, 1); err != nil {
		release()
		return nil, nil, fmt.Errorf("turn file cache off: %w", err)
	}
	return buffer, release, nil
}

// The paired-read instrument must show a slow first read for a file whose
// bytes were written past the cache. The direct reader must return the same
// digest as the cached reader, and must leave the file uncached, which shows
// as a cached first read that is still slow after it.
func diagControlCache(t *testing.T) {
	dir, err := os.MkdirTemp("", "dg")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "uncached")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_, flagErr := unix.FcntlInt(file.Fd(), unix.F_NOCACHE, 1)
	block := make([]byte, 1<<20)
	for i := range block {
		block[i] = byte(i * 31)
	}
	for range 96 {
		if _, err := file.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	syncErr := file.Sync()
	_ = file.Close()
	var split diagSplit
	started := time.Now()
	direct, directErr := diagDigestVariant(t.Context(), path, "direct4m", &split)
	directTime := time.Since(started)
	buffer := make([]byte, 1<<20)
	first, size := diagReadOnce(path, buffer)
	repeat, _ := diagReadOnce(path, buffer)
	cached, cachedErr := diagDigestVariant(t.Context(), path, "split1m", &split)
	if directErr != nil || cachedErr != nil || direct == "" || direct != cached {
		t.Errorf("DIAG CONTROL FAIL direct-reader digest direct=%q cached=%q errors=%v %v", direct, cached, directErr, cachedErr)
	}
	detected := first > 2*repeat && first > 10*time.Millisecond
	t.Logf("DIAG CONTROL uncached-96MiB direct4m=%sms then cached first=%sms repeat=%sms bytes=%d stillUncachedAfterDirect=%v digestsEqual=%v nocacheErr=%v syncErr=%v",
		diagMillis(directTime), diagMillis(first), diagMillis(repeat), size, detected, direct == cached, flagErr, syncErr)
}
