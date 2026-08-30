// Package install embeds the remote service installers used by mesh add.
package install

import _ "embed"

var (
	//go:embed linux.sh
	linux string
	//go:embed darwin.sh
	darwin string
)

// Script returns the installer for a Go operating-system name.
func Script(goos string) (string, bool) {
	switch goos {
	case "linux":
		return linux, true
	case "darwin":
		return darwin, true
	default:
		return "", false
	}
}
