package execution

import (
	"os"

	"golang.org/x/sys/unix"
)

func deviceSync(file *os.File) error {
	_, err := unix.FcntlInt(file.Fd(), unix.F_FULLFSYNC, 0)
	return executionBoundaryError("flush execution namespace device", err)
}
