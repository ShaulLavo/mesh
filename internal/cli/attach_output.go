package cli

import (
	"context"
	"io"
	"os"
	"time"
)

func localAttachOutput(ctx context.Context, output *os.File) (*os.File, func(), error) {
	file, restoreFlags, err := duplicateAttachmentOutput(output)
	if err != nil {
		return nil, nil, err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		_ = file.Close()
	})
	return file, func() {
		if !stop() {
			<-done
		}
		_ = file.Close()
		restoreFlags()
	}, nil
}

func restoreLocalOutput(output *os.File, sequence string) {
	// These escapes are a courtesy, not a reason to hold the terminal hostage.
	_ = output.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	_, _ = io.WriteString(output, sequence)
}
