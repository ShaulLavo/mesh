//go:build darwin

package cli

import (
	"os"
	"strings"
)

// noFollowAnchor maps macOS's system-owned /var and /tmp root firmlinks to
// their /private targets so strict O_NOFOLLOW traversal works under temp dirs.
func noFollowAnchor(clean string) (anchor, rest string) {
	switch {
	case clean == "/var":
		return "/private/var", ""
	case strings.HasPrefix(clean, "/var/"):
		return "/private/var", strings.TrimPrefix(clean, "/var/")
	case clean == "/tmp":
		return "/private/tmp", ""
	case strings.HasPrefix(clean, "/tmp/"):
		return "/private/tmp", strings.TrimPrefix(clean, "/tmp/")
	default:
		sep := string(os.PathSeparator)
		return sep, strings.TrimPrefix(clean, sep)
	}
}
