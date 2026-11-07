package execution

import (
	"golang.org/x/sys/unix"
	"os"
)

func deviceSync(file *os.File) error {
	_, err := unix.FcntlInt(file.Fd(), unix.F_FULLFSYNC, 0)
	return err
}
