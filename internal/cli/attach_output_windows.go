package cli

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func duplicateAttachmentOutput(output *os.File) (*os.File, func(), error) {
	process := windows.CurrentProcess()
	var handle windows.Handle
	if err := windows.DuplicateHandle(process, windows.Handle(output.Fd()), process, &handle, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, nil, fmt.Errorf("duplicate terminal output: %w", err)
	}
	return os.NewFile(uintptr(handle), output.Name()), func() {}, nil
}
