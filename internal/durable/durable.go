// Package durable separates writing a file's changes to its storage device
// from flushing that device's volatile cache, so a durability point that syncs
// several files and directories pays for the cache flush once.
package durable

import "os"

// Sync hands file's data and metadata, or a directory's entries, to the
// storage device. On Darwin that is a plain fsync, which can leave them in
// the drive's volatile cache; Barrier then makes everything synced on that
// device durable. Elsewhere fsync already flushes the device cache.
func Sync(file *os.File) error { return syncFile(file) }

// Barrier flushes the device cache under file, so every earlier Sync on the
// same device survives power loss. It is a no-op where Sync already does that.
func Barrier(file *os.File) error { return barrier(file) }
