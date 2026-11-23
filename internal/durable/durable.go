// Package durable separates writing a file's changes to its storage device
// from flushing that device's volatile cache, so a durability point that syncs
// several files and directories pays for the cache flush once.
//
// The contract: once Barrier returns for a file on some device, every Sync
// that returned earlier for a file or directory on that same device survives
// power loss. On Darwin this assumes that a plain fsync on APFS has written
// the file's data or the directory's entries to the drive before it returns,
// so the drive cache flush covers them. Apple documents that for file data;
// for directory entries it is an assumption the owner accepted on 2026-11-23
// to cut the Mac's flushes from one per file and directory to one per device.
package durable

import "os"

// Sync hands file's data and metadata, or a directory's entries, to the
// storage device. On Darwin that is a plain fsync, which can leave them in
// the drive's volatile cache until Barrier. Elsewhere fsync already flushes
// the device cache.
func Sync(file *os.File) error { return syncFile(file) }

// Barrier flushes the device cache under file, so every earlier Sync on the
// same device survives power loss. It is a no-op where HasBarrier is false.
func Barrier(file *os.File) error { return barrier(file) }
