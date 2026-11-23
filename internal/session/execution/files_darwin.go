package execution

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func renameExclusive(dir *os.File, from, to string) error {
	fd := int(dir.Fd())
	return executionBoundaryError("rename exclusive execution record", unix.RenameatxNp(fd, from, fd, to, unix.RENAME_EXCL))
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
	return fileStamp{dev: uint64(st.Dev), ino: st.Ino, size: st.Size, mtime: unix.Timespec(st.Mtimespec), ctime: unix.Timespec(st.Ctimespec)}, true
}
