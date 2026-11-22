//go:build !linux

package release

import "os"

// Only Linux pins the executing image, so elsewhere no digest is handed over
// and every process hashes its own image.
func imageStamp(os.FileInfo) string { return "" }
