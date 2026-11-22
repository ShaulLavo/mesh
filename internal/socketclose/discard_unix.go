//go:build unix

// Package socketclose helps close a Unix socket without losing what the peer
// has not read yet.
package socketclose

import (
	"io"
	"net"
	"syscall"
)

// DiscardQueued reads whatever the peer has already sent, without waiting.
// Closing a Unix socket that still holds unread input makes the peer read
// ECONNRESET instead of EOF on Linux, and on macOS it also drops the data the
// closing side had already written to the peer, such as a session's final
// exit frame. A client that sent a resize or keystroke just before the close
// would otherwise lose its answer.
//
// It uses Control rather than Read because Close is also how a blocked reader
// is cancelled: Read would wait behind that reader's lock.
func DiscardQueued(stream io.Closer) {
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
