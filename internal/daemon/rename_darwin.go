//go:build darwin

package daemon

import "golang.org/x/sys/unix"

func publishUnixSocket(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}
