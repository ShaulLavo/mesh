//go:build linux

package release

import (
	"fmt"
	"os"
	"syscall"
)

// imageStamp names a file's identity and contents cheaply: device, inode, size,
// modification time and change time. The change time moves on any rewrite and
// cannot be set by a process, so restoring the modification time after a
// same-length write still produces a new stamp. Linux is the only platform
// that pins the executing image through /proc/self/exe; elsewhere the image is
// reopened by path, so no stamp can vouch for the running bytes.
func imageStamp(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d.%d", st.Dev, st.Ino, st.Size, info.ModTime().UnixNano(), st.Ctim.Sec, st.Ctim.Nsec)
}
