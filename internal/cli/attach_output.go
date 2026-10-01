package cli

import (
	"context"
	"os"
	"time"
)

type attachmentFileOutput struct {
	file     *os.File
	ctx      context.Context
	fd       int
	pollFD   int32
	cancelFD int32
}

func (o *attachmentFileOutput) Write(payload []byte) (int, error) {
	return o.write(payload, time.Time{})
}

func (o *attachmentFileOutput) restore(sequence string) {
	// These escapes are a courtesy, not a reason to hold the terminal hostage.
	_, _ = o.write([]byte(sequence), time.Now().Add(100*time.Millisecond))
}
