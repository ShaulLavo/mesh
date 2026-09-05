//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

func discardPendingInput(input *os.File) error {
	fd := input.Fd()
	if fd > math.MaxInt32 {
		return os.ErrInvalid
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	var buf [4096]byte
	for drained := 0; drained < 1<<20; {
		ready, err := unix.Poll(poll, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || ready == 0 {
			return err
		}
		if poll[0].Revents&unix.POLLNVAL != 0 {
			return os.ErrClosed
		}
		n, err := unix.Read(int(fd), buf[:])
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if n == 0 || err != nil {
			return err
		}
		drained += n
	}
	return fmt.Errorf("terminal input remained busy while discarding 1 MiB")
}
