//go:build unix

package transport

import (
	"io"
	"net"
	"syscall"
)

// discardQueuedInput reads whatever the peer has already sent, without
// waiting. Closing a Unix socket that still holds unread input makes the peer
// read ECONNRESET instead of EOF, so an ordinary close would look like a
// failure to a client that sent a resize or keystroke just before it.
//
// It uses Control rather than Read because Close is also how a blocked reader
// is cancelled: Read would wait behind that reader's lock.
func discardQueuedInput(stream io.ReadWriteCloser) {
	unix, ok := stream.(*net.UnixConn)
	if !ok {
		return
	}
	raw, err := unix.SyscallConn()
	if err != nil {
		return
	}
	var scratch [4096]byte
	_ = raw.Control(func(fd uintptr) {
		// A peer that keeps writing cannot hold a close open forever.
		for range maxDiscardReads {
			n, _, err := syscall.Recvfrom(int(fd), scratch[:], syscall.MSG_DONTWAIT)
			if err != nil || n <= 0 {
				return
			}
		}
	})
}

// A Unix socket queues about 200 KiB, so this covers a full queue many times over.
const maxDiscardReads = 256
