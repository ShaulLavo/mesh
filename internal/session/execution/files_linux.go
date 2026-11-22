package execution

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// deviceSync is the final device barrier after the namespace sync. On Linux the
// data fsync and the directory fsyncs that precede it already flush the device
// cache (ext4, XFS and btrfs force their journal with a flush), so a third
// fsync of the same file only repeated that round trip. Darwin keeps its own
// F_FULLFSYNC, which a plain fsync does not provide.
func deviceSync(*os.File) error { return nil }

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
