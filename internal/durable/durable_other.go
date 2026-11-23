//go:build !darwin

package durable

import "os"

func syncFile(file *os.File) error {
	return file.Sync() //nolint:wrapcheck // already an *os.PathError naming the file
}

// On Linux, ext4, XFS and btrfs force their journal with a device flush on
// fsync, so a separate barrier would only repeat that round trip.
func barrier(*os.File) error { return nil }
