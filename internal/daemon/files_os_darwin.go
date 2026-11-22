//go:build darwin

package daemon

import "golang.org/x/sys/unix"

func statFrom(st *unix.Stat_t) fileStat {
	return fileStat{
		dev: uint64(uint32(st.Dev)), ino: st.Ino, size: uint64(st.Size), mode: uint32(st.Mode), //nolint:gosec // device and size are fingerprint bits, never negative
		mtime: st.Mtim.Nano(), ctime: st.Ctim.Nano(),
	}
}

// renameNoReplaceAt renames within one directory and fails with EEXIST when
// the target name exists, as one atomic step.
func renameNoReplaceAt(dirfd int, from, to string) error {
	return wrapRename(unix.RenameatxNp(dirfd, from, dirfd, to, unix.RENAME_EXCL))
}

// exchangeAt atomically swaps two names in one directory.
func exchangeAt(dirfd int, first, second string) error {
	return wrapRename(unix.RenameatxNp(dirfd, first, dirfd, second, unix.RENAME_SWAP))
}
