//go:build !unix

package transport

import "io"

func discardQueuedInput(io.ReadWriteCloser) {}
