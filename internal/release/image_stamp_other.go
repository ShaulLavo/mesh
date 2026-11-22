//go:build !unix

package release

import "os"

// Without a stable file identity no digest is handed over, so every process
// hashes its own image.
func imageStamp(os.FileInfo) string { return "" }
