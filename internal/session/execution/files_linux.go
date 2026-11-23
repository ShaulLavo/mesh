package execution

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func renameExclusive(dir *os.File, from, to string) error {
	fd := int(dir.Fd())
	return executionBoundaryError("rename exclusive execution record", unix.Renameat2(fd, from, fd, to, unix.RENAME_NOREPLACE))
}

// fileStamp identifies a file's current contents and link by identity, size and
// both change times, so any rewrite, replacement or relink produces a new one.
type fileStamp struct {
	dev, ino     uint64
	size         int64
	mtime, ctime unix.Timespec
}

func stampFromInfo(info os.FileInfo) (fileStamp, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileStamp{}, false
	}
	return fileStamp{dev: st.Dev, ino: st.Ino, size: st.Size, mtime: unix.Timespec(st.Mtim), ctime: unix.Timespec(st.Ctim)}, true
}
