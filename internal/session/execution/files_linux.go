package execution

import (
	"os"
)

func deviceSync(file *os.File) error {
	return executionBoundaryError("files_linux.go deviceSync", file.Sync())
}
