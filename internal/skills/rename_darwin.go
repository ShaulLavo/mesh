package skills

import (
	"fmt"
	"golang.org/x/sys/unix"
)

func renameNew(dir int, from, to string) error {
	if err := unix.RenameatxNp(dir, from, dir, to, unix.RENAME_EXCL); err != nil {
		return fmt.Errorf("rename new skill: %w", err)
	}
	return nil
}
