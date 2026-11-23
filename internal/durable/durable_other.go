//go:build !darwin

package durable

import "os"

// HasBarrier is false because fsync already flushes the device cache here:
// ext4, XFS and btrfs force their journal with a device flush, so a separate
// barrier would only repeat that round trip.
const HasBarrier = false

func syncFile(file *os.File) error {
	return file.Sync() //nolint:wrapcheck // already an *os.PathError naming the file
}

func barrier(*os.File) error { return nil }
