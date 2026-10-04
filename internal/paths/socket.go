package paths

import (
	"fmt"
	"runtime"
)

// The native sun_path field also holds a terminating NUL.
func socketPathLimit() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

// ValidateSocketPath reports paths that cannot fit the native Unix socket address.
func ValidateSocketPath(path string) error {
	maximum := socketPathLimit()
	if len(path) > maximum {
		return fmt.Errorf("Unix socket path %q requires %d bytes; maximum is %d bytes on %s; choose a shorter MESH_STATE_DIR on this machine", path, len(path), maximum, runtime.GOOS)
	}
	return nil
}
