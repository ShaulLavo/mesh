//go:build !darwin

package cli

import (
	"os"
	"strings"
)

func noFollowAnchor(clean string) (anchor, rest string) {
	sep := string(os.PathSeparator)
	return sep, strings.TrimPrefix(clean, sep)
}
