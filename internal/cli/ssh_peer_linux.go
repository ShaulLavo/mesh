package cli

import (
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func validateSSHPeer(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("access local SSH peer: %w", err)
	}
	var peer *unix.Ucred
	var lookup error
	if err := raw.Control(func(fd uintptr) { peer, lookup = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return fmt.Errorf("inspect local SSH peer: %w", err)
	}
	if lookup != nil {
		return fmt.Errorf("read local SSH peer credentials: %w", lookup)
	}
	if peer.Uid != uint32(os.Geteuid()) { //nolint:gosec // effective uid uses the OS unsigned representation
		return errors.New("SSH connection owner requires the same local user")
	}
	return nil
}
