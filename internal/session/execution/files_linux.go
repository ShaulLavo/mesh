package execution

import (
	"os"

	"golang.org/x/sys/unix"
)

func deviceSync(file *os.File) error {
	return executionBoundaryError("files_linux.go deviceSync", file.Sync())
}

func renameExclusive(dir *os.File, from, to string) error {
	fd := int(dir.Fd())
	return executionBoundaryError("rename exclusive execution record", unix.Renameat2(fd, from, fd, to, unix.RENAME_NOREPLACE))
}
