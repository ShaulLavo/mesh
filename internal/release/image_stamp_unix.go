//go:build unix

package release

import (
	"fmt"
	"os"
	"syscall"
)

// imageStamp names a file's identity and contents cheaply: device, inode, size
// and modification time. An update replaces the executable by rename, which
// changes the inode, so a stamp match means the same bytes.
func imageStamp(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d:%d", uint64(st.Dev), uint64(st.Ino), st.Size, info.ModTime().UnixNano()) //nolint:gosec,unconvert // Dev is unsigned on Linux and signed on Darwin
}
