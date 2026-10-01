package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func localAttachOutput(ctx context.Context, output *os.File) (*attachmentFileOutput, func(), error) {
	process := windows.CurrentProcess()
	var handle windows.Handle
	if err := windows.DuplicateHandle(process, windows.Handle(output.Fd()), process, &handle, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, nil, fmt.Errorf("duplicate terminal output: %w", err)
	}
	file := os.NewFile(uintptr(handle), output.Name())
	writer := &attachmentFileOutput{file: file, ctx: ctx}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		_ = file.Close()
	})
	return writer, func() {
		if !stop() {
			<-done
		}
		_ = file.Close()
	}, nil
}

func (o *attachmentFileOutput) write(payload []byte, deadline time.Time) (int, error) {
	if !deadline.IsZero() {
		_ = o.file.SetWriteDeadline(deadline)
	}
	n, err := o.file.Write(payload)
	if err != nil {
		return n, fmt.Errorf("write terminal output: %w", err)
	}
	return n, nil
}
