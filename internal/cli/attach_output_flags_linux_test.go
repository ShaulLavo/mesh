//go:build linux

package cli

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAttachConcurrentWriterPrefillsLargePipe(t *testing.T) {
	var capacity int
	drained := exerciseConcurrentPipeWriter(t, func(output *os.File) {
		var err error
		capacity, err = unix.FcntlInt(output.Fd(), unix.F_SETPIPE_SZ, 128<<10)
		if err != nil {
			t.Fatal(err)
		}
		if capacity < 128<<10 {
			t.Fatalf("pipe capacity = %d, want at least 128 KiB", capacity)
		}
	})
	if want := int64(capacity) + 64<<10; drained != want {
		t.Errorf("large-pipe regression drained %d bytes, want full prefill plus peer writes (%d)", drained, want)
	}
}
